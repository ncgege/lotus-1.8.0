package main

import (
	"context"
	scServer "github.com/moran666666/sector-counter/server"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	mux "github.com/gorilla/mux"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/urfave/cli/v2"
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/go-jsonrpc/auth"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/api/apistruct"
	"github.com/filecoin-project/lotus/build"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/lib/ulimit"
	"github.com/filecoin-project/lotus/metrics"
	"github.com/filecoin-project/lotus/node"
	"github.com/filecoin-project/lotus/node/impl"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
	"github.com/filecoin-project/lotus/node/repo"
)

var runCmd = &cli.Command{
	Name:  "run",
	Usage: "Start a lotus miner process",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "c2address",
			Usage: "extern c2 ip and port",
			Value: "",
		},
		&cli.BoolFlag{
			Name:  "multi_wnminer",
			Usage: "enable Multiple winningPoSt miner",
			Value: false,
		},
		&cli.BoolFlag{
			Name:  "wdpost",
			Usage: "enable windowPoSt",
			Value: true,
		},
		&cli.BoolFlag{
			Name:  "wnpost",
			Usage: "enable winningPoSt",
			Value: true,
		},
		&cli.BoolFlag{
			Name:  "p2p",
			Usage: "enable P2P",
			Value: true,
		},
		&cli.StringFlag{
			Name:  "sctype",
			Usage: "sector counter type(alloce,get)",
			Value: "",
		},
		&cli.StringFlag{
			Name:  "sclisten",
			Usage: "host address and port the sector counter will listen on",
			Value: "",
		},
		&cli.StringFlag{
			Name:  "workername",
			Usage: "worker name will display on jobs list",
			Value: "",
		},
		&cli.StringFlag{
			Name:  "ability",
			Usage: "worker sealing ability",
			Value: "AP:1,PC1:1,PC2:1,C1:1,C2:1,FIN:1,GET:1,UNS:1,RD:1",
		},
		&cli.StringFlag{
			Name:  "miner-api",
			Usage: "2345",
		},
		&cli.BoolFlag{
			Name:  "enable-gpu-proving",
			Usage: "enable use of GPU for mining operations",
			Value: true,
		},
		&cli.BoolFlag{
			Name:  "nosync",
			Usage: "don't check full-node sync status",
		},
		&cli.BoolFlag{
			Name:  "manage-fdlimit",
			Usage: "manage open file limit",
			Value: true,
		},
	},
	Action: func(cctx *cli.Context) error {
		if cctx.String("c2address") != "" {
			os.Setenv("C2_ADDRESS", cctx.String("c2address"))
		}

		if cctx.Bool("multi_wnminer") {
			os.Setenv("LOTUS_MUL_WNMINER", "true")
		} else {
			os.Unsetenv("LOTUS_MUL_WNMINER")
		}

		if cctx.Bool("wdpost") {
			os.Setenv("LOTUS_WDPOST", "true")
		} else {
			os.Unsetenv("LOTUS_WDPOST")
		}

		if cctx.Bool("wnpost") {
			os.Setenv("LOTUS_WNPOST", "true")
		} else {
			os.Unsetenv("LOTUS_WNPOST")
		}

		scType := cctx.String("sctype")
		if scType == "alloce" || scType == "get" {
			os.Setenv("SC_TYPE", scType)

			scListen := cctx.String("sclisten")
			if scListen == "" {
				log.Errorf("sclisten must be set")
				return nil
			}
			os.Setenv("SC_LISTEN", scListen)

			if scType == "alloce" {
				scFilePath := filepath.Join(cctx.String(FlagMinerRepo), "sectorid")
				go scServer.Run(scFilePath)
			}
		} else {
			os.Unsetenv("SC_TYPE")
		}

		if cctx.String("workername") != "" {
			os.Setenv("WORKER_NAME", cctx.String("workername"))
		}

		if cctx.String("ability") != "" {
			os.Setenv("ABILITY", cctx.String("ability"))
		}

		if !cctx.Bool("enable-gpu-proving") {
			err := os.Setenv("BELLMAN_NO_GPU", "true")
			if err != nil {
				return err
			}
		}

		nodeApi, ncloser, err := lcli.GetFullNodeAPI(cctx)
		if err != nil {
			return xerrors.Errorf("getting full node api: %w", err)
		}
		defer ncloser()

		ctx, _ := tag.New(lcli.DaemonContext(cctx),
			tag.Insert(metrics.Version, build.BuildVersion),
			tag.Insert(metrics.Commit, build.CurrentCommit),
			tag.Insert(metrics.NodeType, "miner"),
		)
		// Register all metric views
		if err = view.Register(
			metrics.MinerNodeViews...,
		); err != nil {
			log.Fatalf("Cannot register the view: %v", err)
		}
		// Set the metric to one so it is published to the exporter
		stats.Record(ctx, metrics.LotusInfo.M(1))

		v, err := nodeApi.Version(ctx)
		if err != nil {
			return err
		}

		if cctx.Bool("manage-fdlimit") {
			if _, _, err := ulimit.ManageFdLimit(); err != nil {
				log.Errorf("setting file descriptor limit: %s", err)
			}
		}

		if v.APIVersion != api.FullAPIVersion {
			return xerrors.Errorf("lotus-daemon API version doesn't match: expected: %s", api.APIVersion{APIVersion: api.FullAPIVersion})
		}

		log.Info("Checking full node sync status")

		if !cctx.Bool("nosync") {
			if err := lcli.SyncWait(ctx, nodeApi, false); err != nil {
				return xerrors.Errorf("sync wait: %w", err)
			}
		}

		minerRepoPath := cctx.String(FlagMinerRepo)
		r, err := repo.NewFS(minerRepoPath)
		if err != nil {
			return err
		}

		ok, err := r.Exists()
		if err != nil {
			return err
		}
		if !ok {
			return xerrors.Errorf("repo at '%s' is not initialized, run 'lotus-miner init' to set it up", minerRepoPath)
		}

		shutdownChan := make(chan struct{})

		var minerapi api.StorageMiner
		stop, err := node.New(ctx,
			node.StorageMiner(&minerapi),
			node.Override(new(dtypes.ShutdownChan), shutdownChan),
			node.Online(),
			node.Repo(r),

			node.ApplyIf(func(s *node.Settings) bool { return cctx.IsSet("miner-api") },
				node.Override(new(dtypes.APIEndpoint), func() (dtypes.APIEndpoint, error) {
					return multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/" + cctx.String("miner-api"))
				})),
			node.Override(new(api.FullNode), nodeApi),
		)
		if err != nil {
			return xerrors.Errorf("creating node: %w", err)
		}

		endpoint, err := r.APIEndpoint()
		if err != nil {
			return xerrors.Errorf("getting API endpoint: %w", err)
		}

		if cctx.Bool("p2p") {
			// Bootstrap with full node
			remoteAddrs, err := nodeApi.NetAddrsListen(ctx)
			if err != nil {
				return xerrors.Errorf("getting full node libp2p address: %w", err)
			}

			if err := minerapi.NetConnect(ctx, remoteAddrs); err != nil {
				return xerrors.Errorf("connecting to full node (libp2p): %w", err)
			}
		} else {
			log.Warn("This miner will be disable p2p.")
		}

		log.Infof("Remote version %s", v)

		lst, err := manet.Listen(endpoint)
		if err != nil {
			return xerrors.Errorf("could not listen: %w", err)
		}

		mux := mux.NewRouter()

		rpcServer := jsonrpc.NewServer()
		rpcServer.Register("Filecoin", apistruct.PermissionedStorMinerAPI(metrics.MetricedStorMinerAPI(minerapi)))

		mux.Handle("/rpc/v0", rpcServer)
		mux.PathPrefix("/remote").HandlerFunc(minerapi.(*impl.StorageMinerAPI).ServeRemote)
		mux.Handle("/debug/metrics", metrics.Exporter())
		mux.PathPrefix("/").Handler(http.DefaultServeMux) // pprof

		ah := &auth.Handler{
			Verify: minerapi.AuthVerify,
			Next:   mux.ServeHTTP,
		}

		srv := &http.Server{
			Handler: ah,
			BaseContext: func(listener net.Listener) context.Context {
				ctx, _ := tag.New(context.Background(), tag.Upsert(metrics.APIInterface, "lotus-miner"))
				return ctx
			},
		}

		sigChan := make(chan os.Signal, 2)
		go func() {
			select {
			case sig := <-sigChan:
				log.Warnw("received shutdown", "signal", sig)
			case <-shutdownChan:
				log.Warn("received shutdown")
			}

			log.Warn("Shutting down...")
			if err := stop(context.TODO()); err != nil {
				log.Errorf("graceful shutting down failed: %s", err)
			}
			if err := srv.Shutdown(context.TODO()); err != nil {
				log.Errorf("shutting down RPC server failed: %s", err)
			}
			log.Warn("Graceful shutdown successful")
		}()
		signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

		return srv.Serve(manet.NetListener(lst))
	},
}
