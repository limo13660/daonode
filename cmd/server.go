package cmd

import (
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/limo13660/daonode/conf"
	"github.com/limo13660/daonode/core"
	"github.com/limo13660/daonode/limiter"
	"github.com/limo13660/daonode/node"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	config string
	watch  bool
)

const startupRetryInterval = 10 * time.Second

var serverCommand = cobra.Command{
	Use:   "server",
	Short: "Run daonode server",
	Run:   serverHandle,
	Args:  cobra.NoArgs,
}

func init() {
	serverCommand.PersistentFlags().
		StringVarP(&config, "config", "c",
			"/etc/daonode/config.json", "config file path")
	serverCommand.PersistentFlags().
		BoolVarP(&watch, "watch", "w",
			true, "watch file path change")
	command.AddCommand(&serverCommand)
}

func serverHandle(_ *cobra.Command, _ []string) {
	showVersion()
	c := conf.New()
	if err := c.LoadFromPath(config); err != nil {
		log.WithField("err", err).Error("Load config file failed")
		return
	}
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableQuote:     true,
		PadLevelText:     false,
	})
	applyLogConfig(c.LogConfig)

	if c.PprofPort != 0 {
		go func() {
			log.Infof("Starting pprof server on :%d", c.PprofPort)
			if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", c.PprofPort), nil); err != nil {
				log.WithField("err", err).Error("pprof server failed")
			}
		}()
	}

	limiter.Init()
	reloadCh := make(chan struct{}, 1)
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(osSignals)

	nodes, runtimeCore, started := startRuntimeWithRetry(c, reloadCh, osSignals)
	if !started {
		return
	}
	log.Info("Got nodes info from server")
	log.Info("Nodes started")
	defer func() {
		if nodes != nil {
			if err := nodes.Close(); err != nil {
				log.WithField("err", err).Error("Close nodes failed")
			}
		}
		if runtimeCore != nil {
			if err := runtimeCore.Close(); err != nil {
				log.WithField("err", err).Error("Close core failed")
			}
		}
	}()

	if watch {
		if err := c.Watch(config, func() {
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		}); err != nil {
			log.WithField("err", err).Error("Start config watcher failed")
			return
		}
	}
	runtime.GC()

	for {
		select {
		case <-osSignals:
			log.Info("Received exit signal, shutting down")
			return
		case <-reloadCh:
			log.Info("Received reload signal, reloading configuration")
			if err := reload(config, &nodes, &runtimeCore); err != nil {
				log.WithField("err", err).Error("Reload failed")
				return
			}
			log.Info("Reload completed")
		}
	}
}

func startRuntimeWithRetry(
	c *conf.Conf,
	reloadCh chan struct{},
	osSignals <-chan os.Signal,
) (*node.Node, *core.V2Core, bool) {
	for {
		nodes, err := node.New(c.NodeConfigs)
		if err == nil {
			runtimeCore := core.New(c)
			runtimeCore.ReloadCh = reloadCh
			if err = runtimeCore.Start(nodes.NodeInfos); err == nil {
				if err = nodes.Start(c.NodeConfigs, runtimeCore); err == nil {
					return nodes, runtimeCore, true
				}
				_ = nodes.Close()
			}
			_ = runtimeCore.Close()
		}

		log.WithFields(log.Fields{
			"err":      err,
			"retry_in": startupRetryInterval,
		}).Error("Start daonode runtime failed; waiting for a valid panel configuration")

		timer := time.NewTimer(startupRetryInterval)
		select {
		case <-osSignals:
			if !timer.Stop() {
				<-timer.C
			}
			log.Info("Received exit signal while waiting to retry startup")
			return nil, nil, false
		case <-timer.C:
		}
	}
}

func reload(configPath string, nodes **node.Node, runtimeCore **core.V2Core) error {
	newConf := conf.New()
	if err := newConf.LoadFromPath(configPath); err != nil {
		return err
	}
	newNodes, err := node.New(newConf.NodeConfigs)
	if err != nil {
		return err
	}

	var reloadCh chan struct{}
	if *runtimeCore != nil {
		reloadCh = (*runtimeCore).ReloadCh
	}
	if *nodes != nil {
		if err := (*nodes).Close(); err != nil {
			return err
		}
	}
	if *runtimeCore != nil {
		if err := (*runtimeCore).Close(); err != nil {
			return err
		}
	}

	newCore := core.New(newConf)
	newCore.ReloadCh = reloadCh
	if err := newCore.Start(newNodes.NodeInfos); err != nil {
		_ = newCore.Close()
		return err
	}
	if err := newNodes.Start(newConf.NodeConfigs, newCore); err != nil {
		_ = newNodes.Close()
		_ = newCore.Close()
		return err
	}

	applyLogConfig(newConf.LogConfig)
	*nodes = newNodes
	*runtimeCore = newCore
	runtime.GC()
	return nil
}

func applyLogConfig(config conf.LogConfig) {
	switch config.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	}

	var output io.Writer = os.Stdout
	if config.Output != "" {
		file, err := os.OpenFile(config.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
		} else {
			output = file
		}
	}
	oldOutput := log.StandardLogger().Out
	log.SetOutput(output)
	if oldFile, ok := oldOutput.(*os.File); ok && oldFile != os.Stdout && oldFile != os.Stderr && oldFile != output {
		_ = oldFile.Close()
	}
}
