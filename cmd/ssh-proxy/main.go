package main

import (
	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/consuladapter"
	"code.cloudfoundry.org/debugserver"
	loggingclient "code.cloudfoundry.org/diego-logging-client"
	"code.cloudfoundry.org/diego-ssh/authenticators"
	"code.cloudfoundry.org/diego-ssh/cmd/ssh-proxy/config"
	"code.cloudfoundry.org/diego-ssh/healthcheck"
	"code.cloudfoundry.org/diego-ssh/helpers"
	"code.cloudfoundry.org/diego-ssh/proxy"
	"code.cloudfoundry.org/diego-ssh/server"
	"code.cloudfoundry.org/go-loggregator/runtimeemitter"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerflags"
	"code.cloudfoundry.org/locket"
	"errors"
	"flag"
	kb "github.com/SUSE/eirini-ssh/authenticators"
	"github.com/hashicorp/consul/api"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
	"golang.org/x/crypto/ssh"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

var configPath = flag.String(
	"config",
	"",
	"Path to SSH Proxy config.",
)

func main() {
	debugserver.AddFlags(flag.CommandLine)
	flag.Parse()

	sshProxyConfig, err := config.NewSSHProxyConfig(*configPath)
	if err != nil {
		logger, _ := lagerflags.New("ssh-proxy")
		logger.Fatal("failed-to-parse-config", err)
	}

	logger, reconfigurableSink := lagerflags.NewFromConfig("ssh-proxy", sshProxyConfig.LagerConfig)

	metronClient, err := initializeMetron(logger, sshProxyConfig)
	if err != nil {
		logger.Error("failed-to-initialize-metron-client", err)
		os.Exit(1)
	}

	proxySSHServerConfig, err := configureProxy(logger, sshProxyConfig)
	if err != nil {
		logger.Error("configure-failed", err)
		os.Exit(1)
	}

	tlsConfig, err := sshProxyConfig.BackendsTLSConfig()
	if err != nil {
		logger.Error("failed-to-get-tls-config", err)
		os.Exit(1)
	}
	sshProxy := proxy.New(logger, proxySSHServerConfig, metronClient, tlsConfig)
	server := server.NewServer(logger, sshProxyConfig.Address, sshProxy, time.Duration(sshProxyConfig.IdleConnectionTimeout))

	healthCheckHandler := healthcheck.NewHandler(logger)

	members := grouper.Members{
		{"ssh-proxy", server},
	}

	if !sshProxyConfig.DisableHealthCheckServer {
		httpServer := http_server.New(sshProxyConfig.HealthCheckAddress, healthCheckHandler)
		members = append(members, grouper.Member{"healthcheck", httpServer})
	}

	if sshProxyConfig.EnableConsulServiceRegistration {
		consulClient, err := consuladapter.NewClientFromUrl(sshProxyConfig.ConsulCluster)
		if err != nil {
			logger.Fatal("new-client-failed", err)
		}

		registrationRunner := initializeRegistrationRunner(logger, consulClient, sshProxyConfig.Address, clock.NewClock())
		members = append(members, grouper.Member{"registration-runner", registrationRunner})
	}

	if sshProxyConfig.DebugAddress != "" {
		members = append(grouper.Members{{
			"debug-server", debugserver.Runner(sshProxyConfig.DebugAddress, reconfigurableSink),
		}}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)
	monitor := ifrit.Invoke(sigmon.New(group))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
	os.Exit(0)
}

func configureProxy(logger lager.Logger, sshProxyConfig config.SSHProxyConfig) (*ssh.ServerConfig, error) {

	var err error
	permissionsBuilder := kb.NewKubeAuth()

	authens := []authenticators.PasswordAuthenticator{}

	if sshProxyConfig.EnableCFAuth {
		if sshProxyConfig.CCAPIURL == "" {
			return nil, errors.New("ccAPIURL is required for Cloud Foundry authentication")
		}

		_, err = url.Parse(sshProxyConfig.CCAPIURL)
		if err != nil {
			return nil, err
		}

		if sshProxyConfig.UAAPassword == "" {
			return nil, errors.New("UAA password is required for Cloud Foundry authentication")
		}

		if sshProxyConfig.UAAUsername == "" {
			return nil, errors.New("UAA username is required for Cloud Foundry authentication")
		}

		if sshProxyConfig.UAATokenURL == "" {
			return nil, errors.New("uaaTokenURL is required for Cloud Foundry authentication")
		}

		_, err = url.Parse(sshProxyConfig.UAATokenURL)
		if err != nil {
			return nil, err
		}

		client, err := helpers.NewHTTPSClient(sshProxyConfig.SkipCertVerify, []string{sshProxyConfig.UAACACert, sshProxyConfig.CCAPICACert}, time.Duration(sshProxyConfig.CommunicationTimeout))
		if err != nil {
			return nil, err
		}

		cfAuthenticator := authenticators.NewCFAuthenticator(
			logger,
			client,
			sshProxyConfig.CCAPIURL,
			sshProxyConfig.UAATokenURL,
			sshProxyConfig.UAAUsername,
			sshProxyConfig.UAAPassword,
			permissionsBuilder,
		)
		authens = append(authens, cfAuthenticator)
	}

	authenticator := authenticators.NewCompositeAuthenticator(authens...)

	sshConfig := &ssh.ServerConfig{
		ServerVersion:    "SSH-2.0-diego-ssh-proxy",
		PasswordCallback: authenticator.Authenticate,
		AuthLogCallback: func(cmd ssh.ConnMetadata, method string, err error) {
			if err != nil {
				logger.Error("authentication-failed", err, lager.Data{"user": cmd.User()})
			} else {
				logger.Info("authentication-attempted", lager.Data{"user": cmd.User()})
			}
		},
	}

	sshConfig.SetDefaults()

	if sshProxyConfig.HostKey == "" {
		err := errors.New("hostKey is required")
		logger.Fatal("host-key-required", err)
	}

	key, err := parsePrivateKey(logger, sshProxyConfig.HostKey)
	if err != nil {
		logger.Fatal("failed-to-parse-host-key", err)
	}

	sshConfig.AddHostKey(key)

	if sshProxyConfig.AllowedCiphers != "" {
		sshConfig.Config.Ciphers = strings.Split(sshProxyConfig.AllowedCiphers, ",")
	} else {
		sshConfig.Config.Ciphers = []string{"chacha20-poly1305@openssh.com", "aes128-gcm@openssh.com", "aes256-ctr", "aes192-ctr", "aes128-ctr"}
	}

	if sshProxyConfig.AllowedMACs != "" {
		sshConfig.Config.MACs = strings.Split(sshProxyConfig.AllowedMACs, ",")
	} else {
		sshConfig.Config.MACs = []string{"hmac-sha2-256-etm@openssh.com", "hmac-sha2-256"}
	}

	if sshProxyConfig.AllowedKeyExchanges != "" {
		sshConfig.Config.KeyExchanges = strings.Split(sshProxyConfig.AllowedKeyExchanges, ",")
	} else {
		sshConfig.Config.KeyExchanges = []string{"curve25519-sha256@libssh.org"}
	}

	return sshConfig, err
}

func parsePrivateKey(logger lager.Logger, encodedKey string) (ssh.Signer, error) {
	key, err := ssh.ParsePrivateKey([]byte(encodedKey))
	if err != nil {
		logger.Error("failed-to-parse-private-key", err)
		return nil, err
	}
	return key, nil
}

func initializeRegistrationRunner(logger lager.Logger, consulClient consuladapter.Client, listenAddress string, clock clock.Clock) ifrit.Runner {
	_, portString, err := net.SplitHostPort(listenAddress)
	if err != nil {
		logger.Fatal("failed-invalid-listen-address", err)
	}
	portNum, err := net.LookupPort("tcp", portString)
	if err != nil {
		logger.Fatal("failed-invalid-listen-port", err)
	}

	registration := &api.AgentServiceRegistration{
		Name: "ssh-proxy",
		Port: portNum,
		Check: &api.AgentServiceCheck{
			TTL: "20s",
		},
	}

	return locket.NewRegistrationRunner(logger, registration, consulClient, locket.RetryInterval, clock)
}

func initializeMetron(logger lager.Logger, locketConfig config.SSHProxyConfig) (loggingclient.IngressClient, error) {
	client, err := loggingclient.NewIngressClient(locketConfig.LoggregatorConfig)
	if err != nil {
		return nil, err
	}

	if locketConfig.LoggregatorConfig.UseV2API {
		emitter := runtimeemitter.NewV1(client)
		go emitter.Run()
	}

	return client, nil
}
