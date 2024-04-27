package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andydunstall/pico/pkg/log"
	"github.com/andydunstall/pico/server/config"
	"github.com/andydunstall/pico/server/gossip"
	"github.com/andydunstall/pico/server/netmap"
	"github.com/andydunstall/pico/server/proxy"
	adminserver "github.com/andydunstall/pico/server/server/admin"
	proxyserver "github.com/andydunstall/pico/server/server/proxy"
	"github.com/hashicorp/go-sockaddr"
	rungroup "github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "start a server node",
		Long: `Start a server node.

The Pico server is responsible for proxying requests from downstream clients to
registered upstream listeners.

The server has two ports, a 'proxy' port which accepts connections from both
downstream clients and upstream listeners, and an 'admin' port which is used
to inspect the status of the server.

Pico may run as a cluster of nodes for fault tolerance and scalability. Use
'--cluster.join' to configure addresses of existing members in the cluster
to join.

Examples:
  # Start a Pico server.
  pico server

  # Start a Pico server, listening for proxy connections on :7000 and admin
  // ocnnections on :9000.
  pico server --proxy.bind-addr :8000 --admin.bind-addr :9000

  # Start a Pico server and join an existing cluster by specifying each member.
  pico server --cluster.join 10.26.104.14,10.26.104.75

  # Start a Pico server and join an existing cluster by specifying a domain.
  # The server will resolve the domain and attempt to join each returned
  # member.
  pico server --cluster.join cluster.pico-ns.svc.cluster.local
`,
	}

	var conf config.Config

	cmd.Flags().StringVar(
		&conf.Proxy.BindAddr,
		"proxy.bind-addr",
		":8080",
		`
The host/port to listen for incoming proxy HTTP and WebSocket connections.

If the host is unspecified it defaults to all listeners, such as
'--proxy.bind-addr :8080' will listen on '0.0.0.0:8080'`,
	)
	cmd.Flags().StringVar(
		&conf.Proxy.AdvertiseAddr,
		"proxy.advertise-addr",
		"",
		`
Proxy listen address to advertise to other nodes in the cluster. This is the
address other nodes will used to forward proxy requests.

Such as if the listen address is ':8080', the advertised address may be
'10.26.104.45:8080' or 'node1.cluster:8080'.

By default, if the bind address includes an IP to bind to that will be used.
If the bind address does not include an IP (such as ':8080') the nodes
private IP will be used, such as a bind address of ':8080' may have an
advertise address of '10.26.104.14:8080'.`,
	)
	cmd.Flags().IntVar(
		&conf.Proxy.GatewayTimeout,
		"proxy.gateway-timeout",
		15,
		`
The timeout when sending proxied requests to upstream listeners for forwarding
to other nodes in the cluster.
If the upstream does not respond within the given timeout a
'504 Gateway Timeout' is returned to the client.`,
	)

	cmd.Flags().StringVar(
		&conf.Admin.BindAddr,
		"admin.bind-addr",
		":8081",
		`
The host/port to listen for incoming admin connections.

If the host is unspecified it defaults to all listeners, such as
'--admin.bind-addr :8081' will listen on '0.0.0.0:8081'`,
	)
	cmd.Flags().StringVar(
		&conf.Admin.AdvertiseAddr,
		"admin.advertise-addr",
		"",
		`
Admin listen address to advertise to other nodes in the cluster. This is the
address other nodes will used to forward admin requests.

Such as if the listen address is ':8081', the advertised address may be
'10.26.104.45:8081' or 'node1.cluster:8081'.

By default, if the bind address includes an IP to bind to that will be used.
If the bind address does not include an IP (such as ':8081') the nodes
private IP will be used, such as a bind address of ':8081' may have an
advertise address of '10.26.104.14:8081'.`,
	)

	cmd.Flags().StringVar(
		&conf.Gossip.BindAddr,
		"gossip.bind-addr",
		":7000",
		`
The host/port to listen for inter-node gossip traffic.

If the host is unspecified it defaults to all listeners, such as
'--gossip.bind-addr :7000' will listen on '0.0.0.0:7000'`,
	)

	cmd.Flags().StringVar(
		&conf.Gossip.AdvertiseAddr,
		"gossip.advertise-addr",
		"",
		`
Gossip listen address to advertise to other nodes in the cluster. This is the
address other nodes will used to gossip with the node.

Such as if the listen address is ':7000', the advertised address may be
'10.26.104.45:7000' or 'node1.cluster:7000'.

By default, if the bind address includes an IP to bind to that will be used.
If the bind address does not include an IP (such as ':7000') the nodes
private IP will be used, such as a bind address of ':7000' may have an
advertise address of '10.26.104.14:7000'.`,
	)

	cmd.Flags().IntVar(
		&conf.Server.GracefulShutdownTimeout,
		"server.graceful-shutdown-timeout",
		60,
		`
Maximum number of seconds after a shutdown signal is received (SIGTERM or
SIGINT) to gracefully shutdown the server node before terminating.
This includes handling in-progress HTTP requests, gracefully closing
connections to upstream listeners, announcing to the cluster the node is
leaving...`,
	)

	cmd.Flags().StringVar(
		&conf.Cluster.NodeID,
		"cluster.node-id",
		"",
		`
A unique identifier for the node in the cluster.

By default a random ID will be generated for the node.`,
	)
	cmd.Flags().StringSliceVar(
		&conf.Cluster.Join,
		"cluster.join",
		nil,
		`
A list of addresses of members in the cluster to join.

This may be either addresses of specific nodes, such as
'--cluster.join 10.26.104.14,10.26.104.75', or a domain that resolves to
the addresses of the nodes in the cluster (e.g. a Kubernetes headless
service), such as '--cluster.join pico.prod-pico-ns'.

Each address must include the host, and may optionally include a port. If no
port is given, the gossip port of this node is used.

Note each node propagates membership information to the other known nodes,
so the initial set of configured members only needs to be a subset of nodes.`,
	)

	cmd.Flags().StringVar(
		&conf.Log.Level,
		"log.level",
		"info",
		`
Minimum log level to output.

The available levels are 'debug', 'info', 'warn' and 'error'.`,
	)
	cmd.Flags().StringSliceVar(
		&conf.Log.Subsystems,
		"log.subsystems",
		nil,
		`
Each log has a 'subsystem' field where the log occured.

'--log.subsystems' enables all log levels for those given subsystems. This
can be useful to debug a particular subsystem without having to enable all
debug logs.

Such as you can enable 'gossip' logs with '--log.subsystems gossip'.`,
	)

	cmd.Run = func(cmd *cobra.Command, args []string) {
		if err := conf.Validate(); err != nil {
			fmt.Printf("invalid config: %s\n", err.Error())
			os.Exit(1)
		}

		logger, err := log.NewLogger(conf.Log.Level, conf.Log.Subsystems)
		if err != nil {
			fmt.Printf("failed to setup logger: %s\n", err.Error())
			os.Exit(1)
		}

		if conf.Cluster.NodeID == "" {
			conf.Cluster.NodeID = netmap.GenerateNodeID()
		}

		if conf.Proxy.AdvertiseAddr == "" {
			advertiseAddr, err := advertiseAddrFromBindAddr(conf.Proxy.BindAddr)
			if err != nil {
				logger.Error("invalid configuration", zap.Error(err))
				os.Exit(1)
			}
			conf.Proxy.AdvertiseAddr = advertiseAddr
		}
		if conf.Admin.AdvertiseAddr == "" {
			advertiseAddr, err := advertiseAddrFromBindAddr(conf.Admin.BindAddr)
			if err != nil {
				logger.Error("invalid configuration", zap.Error(err))
				os.Exit(1)
			}
			conf.Admin.AdvertiseAddr = advertiseAddr
		}
		if conf.Gossip.AdvertiseAddr == "" {
			advertiseAddr, err := advertiseAddrFromBindAddr(conf.Gossip.BindAddr)
			if err != nil {
				logger.Error("invalid configuration", zap.Error(err))
				os.Exit(1)
			}
			conf.Gossip.AdvertiseAddr = advertiseAddr
		}

		if err := run(&conf, logger); err != nil {
			logger.Error("failed to run server", zap.Error(err))
			os.Exit(1)
		}
	}

	return cmd
}

func run(conf *config.Config, logger log.Logger) error {
	logger.Info("starting pico server", zap.Any("conf", conf))

	registry := prometheus.NewRegistry()
	adminServer := adminserver.NewServer(
		conf.Admin.BindAddr,
		registry,
		logger,
	)

	networkMap := netmap.NewNetworkMap(&netmap.Node{
		ID:        conf.Cluster.NodeID,
		ProxyAddr: conf.Proxy.AdvertiseAddr,
		AdminAddr: conf.Admin.AdvertiseAddr,
	}, logger)
	networkMap.Metrics().Register(registry)
	adminServer.AddStatus("/netmap", netmap.NewStatus(networkMap))

	gossiper, err := gossip.NewGossip(networkMap, conf, logger)
	if err != nil {
		return fmt.Errorf("gossip: %w", err)
	}
	defer gossiper.Close()
	adminServer.AddStatus("/gossip", gossip.NewStatus(gossiper))

	// Attempt to join an existing cluster. Note if 'join' is a domain that
	// doesn't map to any entries (except ourselves), then join will succeed
	// since it means we're the first member.
	nodeIDs, err := gossiper.Join(conf.Cluster.Join)
	if err != nil {
		return fmt.Errorf("join cluster: %w", err)
	}
	if len(nodeIDs) > 0 {
		logger.Info(
			"joined cluster",
			zap.Strings("node-ids", nodeIDs),
		)
	}

	p := proxy.NewProxy(networkMap, registry, logger)
	proxyServer := proxyserver.NewServer(
		conf.Proxy.BindAddr,
		p,
		&conf.Proxy,
		registry,
		logger,
	)
	adminServer.AddStatus("/proxy", proxy.NewStatus(p))

	var group rungroup.Group

	// Termination handler.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	group.Add(func() error {
		sig := <-signalCh
		logger.Info(
			"received shutdown signal",
			zap.String("signal", sig.String()),
		)

		leaveCtx, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(conf.Server.GracefulShutdownTimeout)*time.Second,
		)
		defer cancel()

		// Leave as soon as we receive the shutdown signal to avoid receiving
		// forward proxy requests.
		if err := gossiper.Leave(leaveCtx); err != nil {
			logger.Warn("failed to gracefully leave cluster", zap.Error(err))
		} else {
			logger.Info("left cluster")
		}

		return nil
	}, func(error) {
	})

	// Proxy server.
	group.Add(func() error {
		if err := proxyServer.Serve(); err != nil {
			return fmt.Errorf("proxy server serve: %w", err)
		}
		return nil
	}, func(error) {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(conf.Server.GracefulShutdownTimeout)*time.Second,
		)
		defer cancel()

		if err := proxyServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("failed to gracefully shutdown proxy server", zap.Error(err))
		}

		logger.Info("proxy server shut down")
	})

	// Admin server.
	group.Add(func() error {
		if err := adminServer.Serve(); err != nil {
			return fmt.Errorf("admin server serve: %w", err)
		}
		return nil
	}, func(error) {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(conf.Server.GracefulShutdownTimeout)*time.Second,
		)
		defer cancel()

		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("failed to gracefully shutdown server", zap.Error(err))
		}

		logger.Info("admin server shut down")
	})

	if err := group.Run(); err != nil {
		return err
	}

	logger.Info("shutdown complete")

	return nil
}

func advertiseAddrFromBindAddr(bindAddr string) (string, error) {
	if strings.HasPrefix(bindAddr, ":") {
		bindAddr = "0.0.0.0" + bindAddr
	}

	host, port, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return "", fmt.Errorf("invalid bind addr: %s: %w", bindAddr, err)
	}

	if host == "0.0.0.0" {
		ip, err := sockaddr.GetPrivateIP()
		if err != nil {
			return "", fmt.Errorf("get interface addr: %w", err)
		}
		if ip == "" {
			return "", fmt.Errorf("no private ip found")
		}
		return ip + ":" + port, nil
	}
	return bindAddr, nil
}
