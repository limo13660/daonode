package cmd

import (
	"errors"
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

const (
	startupRetryInterval = 10 * time.Second
	reloadRetryBase      = 10 * time.Second
	reloadRetryMaximum   = 5 * time.Minute
)

type serverRuntime struct {
	config   *conf.Conf
	nodes    *node.Node
	core     *core.V2Core
	snapshot *node.Snapshot
}

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
	snapshot, err := nodes.Snapshot()
	if err != nil {
		log.WithField("err", err).Error("Capture initial runtime snapshot failed")
		_ = nodes.Close()
		_ = runtimeCore.Close()
		return
	}
	state := &serverRuntime{
		config:   c,
		nodes:    nodes,
		core:     runtimeCore,
		snapshot: snapshot,
	}
	log.Info("Got nodes info from server")
	log.Info("Nodes started")
	defer func() {
		if err := state.closeActive(); err != nil {
			log.WithField("err", err).Error("Close runtime failed")
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
	reloadFailures := 0

	for {
		select {
		case <-osSignals:
			log.Info("Received exit signal, shutting down")
			return
		case <-reloadCh:
			log.Info("Received reload signal, reloading configuration")
			if err := reload(config, state, reloadCh); err != nil {
				log.WithField("err", err).Error("Reload failed")
				if !state.running() && !restoreRuntimeWithRetry(state, reloadCh, osSignals) {
					return
				}
				reloadFailures++
				drainReloadSignals(reloadCh)
				delay := reloadRetryDelay(reloadFailures)
				log.WithField("retry_in", delay).Warn("Keeping last-known-good runtime and scheduling reload retry")
				scheduleReload(reloadCh, delay)
				continue
			}
			reloadFailures = 0
			drainReloadSignals(reloadCh)
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
			var runtimeCore *core.V2Core
			runtimeCore, err = startPreparedRuntime(c, nodes, reloadCh)
			if err == nil {
				return nodes, runtimeCore, true
			}
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

func reload(configPath string, state *serverRuntime, reloadCh chan struct{}) error {
	if state == nil || !state.running() {
		return fmt.Errorf("active runtime is unavailable")
	}
	newConf := conf.New()
	if err := newConf.LoadFromPath(configPath); err != nil {
		return err
	}
	newNodes, err := node.New(newConf.NodeConfigs)
	if err != nil {
		return err
	}
	candidateSnapshot, err := newNodes.Snapshot()
	if err != nil {
		_ = newNodes.Close()
		return fmt.Errorf("snapshot prepared candidate: %w", err)
	}
	validationCore := core.New(newConf)
	if err := validationCore.Start(newNodes.NodeInfos); err != nil {
		_ = newNodes.Close()
		return fmt.Errorf("validate candidate runtime: %w", err)
	}
	for _, candidate := range candidateSnapshot.Nodes {
		if err := validationCore.ValidateRuntime(candidate.Info.Tag, candidate.Info, candidate.Users); err != nil {
			_ = newNodes.Close()
			return fmt.Errorf("validate candidate runtime: %w", err)
		}
	}
	lastKnownGood, err := state.nodes.Snapshot()
	if err != nil {
		_ = newNodes.Close()
		return fmt.Errorf("snapshot active runtime: %w", err)
	}
	previousConfig := state.config
	state.snapshot = lastKnownGood

	if err := state.closeActive(); err != nil {
		_ = newNodes.Close()
		restoreErr := state.restore(reloadCh)
		return errors.Join(
			fmt.Errorf("stop active runtime: %w", err),
			wrapRestoreError(restoreErr),
		)
	}

	newCore, err := startPreparedRuntime(newConf, newNodes, reloadCh)
	if err != nil {
		state.config = previousConfig
		restoreErr := state.restore(reloadCh)
		return errors.Join(
			fmt.Errorf("activate candidate runtime: %w", err),
			wrapRestoreError(restoreErr),
		)
	}

	applyLogConfig(newConf.LogConfig)
	state.config = newConf
	state.nodes = newNodes
	state.core = newCore
	state.snapshot = candidateSnapshot
	runtime.GC()
	return nil
}

func startPreparedRuntime(c *conf.Conf, nodes *node.Node, reloadCh chan struct{}) (*core.V2Core, error) {
	if c == nil || nodes == nil {
		return nil, fmt.Errorf("prepared runtime is incomplete")
	}
	runtimeCore := core.New(c)
	runtimeCore.ReloadCh = reloadCh
	if err := runtimeCore.Start(nodes.NodeInfos); err != nil {
		_ = nodes.Close()
		_ = runtimeCore.Close()
		return nil, err
	}
	if err := nodes.Start(c.NodeConfigs, runtimeCore); err != nil {
		_ = nodes.Close()
		_ = runtimeCore.Close()
		return nil, err
	}
	return runtimeCore, nil
}

func (s *serverRuntime) running() bool {
	return s != nil && s.nodes != nil && s.core != nil
}

func (s *serverRuntime) closeActive() error {
	if s == nil {
		return nil
	}
	var closeErrors []error
	if s.nodes != nil {
		if err := s.nodes.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close nodes: %w", err))
		}
	}
	if s.core != nil {
		if err := s.core.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close core: %w", err))
		}
	}
	s.nodes = nil
	s.core = nil
	return errors.Join(closeErrors...)
}

func (s *serverRuntime) restore(reloadCh chan struct{}) error {
	if s == nil || s.config == nil || s.snapshot == nil {
		return fmt.Errorf("last-known-good runtime is unavailable")
	}
	restoredNodes, err := node.NewFromSnapshot(s.snapshot)
	if err != nil {
		return err
	}
	restoredCore, err := startPreparedRuntime(s.config, restoredNodes, reloadCh)
	if err != nil {
		return err
	}
	s.nodes = restoredNodes
	s.core = restoredCore
	log.Info("Last-known-good runtime restored")
	return nil
}

func restoreRuntimeWithRetry(
	state *serverRuntime,
	reloadCh chan struct{},
	osSignals <-chan os.Signal,
) bool {
	for !state.running() {
		timer := time.NewTimer(startupRetryInterval)
		select {
		case <-osSignals:
			if !timer.Stop() {
				<-timer.C
			}
			log.Info("Received exit signal while waiting to restore the last-known-good runtime")
			return false
		case <-timer.C:
		}
		if err := state.restore(reloadCh); err != nil {
			log.WithFields(log.Fields{
				"err":      err,
				"retry_in": startupRetryInterval,
			}).Error("Restore last-known-good runtime failed")
		}
	}
	return true
}

func wrapRestoreError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("restore last-known-good runtime: %w", err)
}

func reloadRetryDelay(failures int) time.Duration {
	if failures <= 1 {
		return reloadRetryBase
	}
	delay := reloadRetryBase
	for i := 1; i < failures && delay < reloadRetryMaximum; i++ {
		if delay > reloadRetryMaximum/2 {
			return reloadRetryMaximum
		}
		delay *= 2
	}
	if delay > reloadRetryMaximum {
		return reloadRetryMaximum
	}
	return delay
}

func scheduleReload(reloadCh chan struct{}, delay time.Duration) {
	time.AfterFunc(delay, func() {
		select {
		case reloadCh <- struct{}{}:
		default:
		}
	})
}

func drainReloadSignals(reloadCh chan struct{}) {
	for {
		select {
		case <-reloadCh:
		default:
			return
		}
	}
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
