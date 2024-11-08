/*
 * Copyright (C) 2020-2022, IrineSistiana
 *
 * This file is part of mosdns.
 *
 * mosdns is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * mosdns is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package coremain

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/kardianos/service"
	"github.com/mitchellh/mapstructure"
	"github.com/radovskyb/watcher"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/IrineSistiana/mosdns/v5/mlog"
)

type serverFlags struct {
	c         string
	dir       string
	cpu       int
	asService bool
}

var rootCmd = &cobra.Command{
	Use: "mosdns",
}

func init() {
	sf := new(serverFlags)
	startCmd := &cobra.Command{
		Use:   "start [-c config_file] [-d working_dir]",
		Short: "Start mosdns main program.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if sf.asService {
				svc, err := service.New(&serverService{f: sf}, svcCfg)
				if err != nil {
					return fmt.Errorf("failed to init service, %w", err)
				}
				return svc.Run()
			}

			var m *Mosdns
			var handler = func(sf *serverFlags) {
				var err error
				m, err = NewServer(sf)
				if err != nil {
					return
				}

				go func() {
					c := make(chan os.Signal, 1)
					signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
					sig := <-c
					m.logger.Warn("signal received", zap.Stringer("signal", sig))
					m.sc.SendCloseSignal(nil)
				}()

				m.GetSafeClose().WaitClosed()

			}
			go handler(sf)

			w := watcher.New()

			// 设置监听模式为所有事件
			w.SetMaxEvents(1)
			w.FilterOps(watcher.Write, watcher.Create, watcher.Remove, watcher.Rename, watcher.Move)

			defer w.Close()

			go func() {
				for {
					select {
					case event := <-w.Event:
						mlog.L().Info("server reload by config change")
						m.sc.SendCloseSignal(nil)
						mlog.L().Info("config change:", zap.String("file", event.Path))
						go handler(sf)

					case err := <-w.Error:
						log.Fatalln(err)
					case <-w.Closed:
						return
					}
				}
			}()

			if err := w.Add("/data/mosdns/config.yaml"); err != nil {
				log.Fatalln(err)
			}

			go func() {
				if err := w.Start(time.Millisecond * 100); err != nil {
					log.Fatalln(err)
				}
			}()

			quit := make(chan os.Signal)
			signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
			<-quit
			log.Println("Shuting down server...")

			log.Println("Server exiting")

			return nil
		},
		DisableFlagsInUseLine: true,
		SilenceUsage:          true,
	}
	rootCmd.AddCommand(startCmd)
	fs := startCmd.Flags()
	fs.StringVarP(&sf.c, "config", "c", "", "config file")
	fs.StringVarP(&sf.dir, "dir", "d", "", "working dir")
	fs.IntVar(&sf.cpu, "cpu", 0, "set runtime.GOMAXPROCS")
	fs.BoolVar(&sf.asService, "as-service", false, "start as a service")
	_ = fs.MarkHidden("as-service")

	serviceCmd := &cobra.Command{
		Use:   "service",
		Short: "Manage mosdns as a system service.",
	}
	serviceCmd.PersistentPreRunE = initService
	serviceCmd.AddCommand(
		newSvcInstallCmd(),
		newSvcUninstallCmd(),
		newSvcStartCmd(),
		newSvcStopCmd(),
		newSvcRestartCmd(),
		newSvcStatusCmd(),
	)
	rootCmd.AddCommand(serviceCmd)
}

func AddSubCmd(c *cobra.Command) {
	rootCmd.AddCommand(c)
}

func Run() error {
	return rootCmd.Execute()
}

func NewServer(sf *serverFlags) (*Mosdns, error) {
	if sf.cpu > 0 {
		runtime.GOMAXPROCS(sf.cpu)
	}

	if len(sf.dir) > 0 {
		err := os.Chdir(sf.dir)
		if err != nil {
			return nil, fmt.Errorf("failed to change the current working directory, %w", err)
		}
		mlog.L().Info("working directory changed", zap.String("path", sf.dir))
	}

	cfg, fileUsed, err := loadConfig(sf.c)
	if err != nil {
		return nil, fmt.Errorf("fail to load config, %w", err)
	}
	mlog.L().Info("main config loaded", zap.String("file", fileUsed))

	return NewMosdns(cfg)
}

// loadConfig load a config from a file. If filePath is empty, it will
// automatically search and load a file which name start with "config".
func loadConfig(filePath string) (*Config, string, error) {
	v := viper.New()

	if len(filePath) > 0 {
		v.SetConfigFile(filePath)
	} else {
		v.SetConfigName("config")
		v.AddConfigPath(".")
	}

	if err := v.ReadInConfig(); err != nil {
		return nil, "", fmt.Errorf("failed to read config: %w", err)
	}

	decoderOpt := func(cfg *mapstructure.DecoderConfig) {
		cfg.ErrorUnused = true
		cfg.TagName = "yaml"
		cfg.WeaklyTypedInput = true
	}

	cfg := new(Config)
	if err := v.Unmarshal(cfg, decoderOpt); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal config: %w", err)
	}
	return cfg, v.ConfigFileUsed(), nil
}
