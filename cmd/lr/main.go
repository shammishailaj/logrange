// Copyright 2018-2019 The logrange Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"context"
	"fmt"
	"github.com/jrivets/log4g"
	"github.com/logrange/logrange"
	"github.com/logrange/logrange/client"
	"github.com/logrange/logrange/client/collector"
	"github.com/logrange/logrange/client/forwarder"
	"github.com/logrange/logrange/client/shell"
	"github.com/logrange/logrange/cmd"
	"github.com/logrange/logrange/pkg/storage"
	"github.com/logrange/logrange/pkg/utils"
	"github.com/logrange/range/pkg/utils/fileutil"
	ucli "gopkg.in/urfave/cli.v2"
	"os"
	"path"
	"sort"
	"strings"
)

const (
	argCfgFile       = "config-file"
	argLogCfgFile    = "log-config-file"
	argServerAddr    = "server-addr"
	argStorageDir    = "storage-dir"
	argStartAsDaemon = "daemon"

	argQueryStreamMode = "pipe-mode"
)

var (
	logger = log4g.GetLogger("lr")
)

// main function is an entry point for 'lr' command. The lr is logrange client, which groups
// different functionalities in one executable. The functionalities are:
// 		shell 	- is an interactive CLI to run commands for logrange
//		forward	- data forwarding functionality. Holds running console, but runs as background process.
// 		collect - data collection functionality. It scans local files and sends the data to logrange.
// 		query   -
func main() {
	defer log4g.Shutdown()

	cmnFlags := []ucli.Flag{
		&ucli.StringFlag{
			Name:  argServerAddr,
			Usage: "server address",
		},
		&ucli.StringFlag{
			Name:  argStorageDir,
			Usage: "storage directory",
		},
		&ucli.StringFlag{
			Name:  argCfgFile,
			Usage: "configuration file path",
		},
		&ucli.StringFlag{
			Name:  argLogCfgFile,
			Usage: "log4g configuration file path",
		},
		&ucli.BoolFlag{
			Name:  argStartAsDaemon,
			Usage: "starting collector or forwarder as daemon (detached from the console)",
		},
	}

	app := &ucli.App{
		Name:    "lr",
		Version: logrange.Version,
		Usage:   "Logrange client",
		Commands: []*ucli.Command{
			{
				Name:   "collect",
				Usage:  "Run data collection",
				Action: runCollector,
				Flags:  cmnFlags,
			},
			{
				Name:   "forward",
				Usage:  "Run data forwarding",
				Action: runForwarder,
				Flags:  cmnFlags,
			},
			{
				Name:   "stop-collect",
				Usage:  "Stop data collection",
				Action: stopCollector,
				Flags:  []ucli.Flag{cmnFlags[1], cmnFlags[2]},
			},
			{
				Name:   "stop-forward",
				Usage:  "Stop data forwarding",
				Action: stopForwarder,
				Flags:  []ucli.Flag{cmnFlags[1], cmnFlags[2]},
			},
			{
				Name:   "shell",
				Usage:  "Run lql shell",
				Action: runShell,
				Flags:  []ucli.Flag{cmnFlags[0]},
			},
			{
				Name:      "query",
				Usage:     "Execute lql query",
				Action:    execQuery,
				ArgsUsage: "[lql query]",
				Flags: []ucli.Flag{cmnFlags[0],
					&ucli.BoolFlag{
						Name:  argQueryStreamMode,
						Usage: "enable query pipe mode (blocking)",
					},
				},
			},
		},
	}

	sort.Sort(ucli.FlagsByName(app.Flags))
	for _, cmd := range app.Commands {
		sort.Sort(ucli.FlagsByName(cmd.Flags))
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err)
	}
}

func initCfg(c *ucli.Context) (*client.Config, error) {
	var (
		err error
		cfg = client.NewDefaultConfig()
	)

	logCfgFile := c.String(argLogCfgFile)
	if logCfgFile != "" {
		err = log4g.ConfigF(logCfgFile)
		if err != nil {
			return nil, err
		}
	}

	cfgFile := c.String(argCfgFile)
	if cfgFile != "" {
		logger.Info("Loading config from=", cfgFile)
		config, err := client.LoadCfgFromFile(cfgFile)
		if err != nil {
			return nil, err
		}
		cfg.Apply(config)
	}

	applyArgsToCfg(c, cfg)
	return cfg, nil
}

// pidFileName returns the name of file, where the client process pid will be stored
func pidFileName(cname string, cfg *client.Config) string {
	if cfg.Storage.Type == storage.TypeInMem {
		return ""
	}
	err := fileutil.EnsureDirExists(cfg.Storage.Location)
	if err != nil {
		fmt.Println("Error: the folder ", cfg.Storage.Location, " could not be created err=", err)
		return ""
	}
	return path.Join(cfg.Storage.Location, cname+".pid")
}

func applyArgsToCfg(c *ucli.Context, cfg *client.Config) {
	if sa := c.String(argServerAddr); sa != "" {
		cfg.Transport.ListenAddr = sa
	}
	if sd := c.String(argStorageDir); sd != "" {
		cfg.Storage.Type = storage.TypeFile
		cfg.Storage.Location = sd
	}
}

func newCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	utils.NewNotifierOnIntTermSignal(func(s os.Signal) {
		logger.Warn("Handling signal=", s)
		cancel()
	})
	return ctx
}

//===================== collector =====================

func runCollector(c *ucli.Context) error {
	cfg, err := initCfg(c)
	if err != nil {
		return err
	}

	pfn := pidFileName("collector", cfg)
	if c.Bool(argStartAsDaemon) {
		if pfn == "" {
			fmt.Println("Warning: starting as daemon with in-mem storage. There will be no way to stop it via lr command.")
		}
		res := cmd.RemoveArgsWithName(os.Args[1:], argStartAsDaemon)
		return cmd.RunCommand(os.Args[0], res...)
	}

	if pfn != "" {
		pf := cmd.NewPidFile(pfn)
		if !pf.Lock() {
			return fmt.Errorf("already running?")
		}
		defer pf.Unlock()
	}

	cli, err := client.NewClient(*cfg.Transport)
	if err != nil {
		return err
	}

	defer cli.Close()
	strg, err := client.NewStorage(cfg.Storage)
	if err != nil {
		return err
	}

	return collector.Run(newCtx(), cfg.Collector, cli, strg)
}

func stopCollector(c *ucli.Context) error {
	cfg, err := initCfg(c)
	if err != nil {
		return err
	}

	pfn := pidFileName("collector", cfg)
	if pfn == "" {
		return fmt.Errorf("could not determine collector pid, the configuration doesn't have permanent storage, in-mem only")
	}

	pf := cmd.NewPidFile(pfn)
	return pf.Interrupt()
}

//===================== forwarder =====================

func runForwarder(c *ucli.Context) error {
	cfg, err := initCfg(c)
	if err != nil {
		return err
	}

	pfn := pidFileName("forwarder", cfg)
	if c.Bool(argStartAsDaemon) {
		if pfn == "" {
			fmt.Println("Warning: starting as daemon with in-mem storage. There will be no way to stop it via lr command.")
		}
		res := cmd.RemoveArgsWithName(os.Args[1:], argStartAsDaemon)
		return cmd.RunCommand(os.Args[0], res...)
	}

	if pfn != "" {
		pf := cmd.NewPidFile(pfn)
		if !pf.Lock() {
			return fmt.Errorf("already running?")
		}
		defer pf.Unlock()
	}

	cli, err := client.NewClient(*cfg.Transport)
	if err != nil {
		return err
	}

	defer cli.Close()
	strg, err := client.NewStorage(cfg.Storage)
	if err != nil {
		return err
	}

	return forwarder.Run(newCtx(), cfg.Forwarder, cli, strg)
}

func stopForwarder(c *ucli.Context) error {
	cfg, err := initCfg(c)
	if err != nil {
		return err
	}

	pfn := pidFileName("forwarder", cfg)
	if pfn == "" {
		return fmt.Errorf("could not determine forwarder pid, the configuration doesn't have permanent storage, in-mem only")
	}

	pf := cmd.NewPidFile(pfn)
	return pf.Interrupt()
}

//===================== query =====================

func execQuery(c *ucli.Context) error {
	log4g.SetLogLevel("", log4g.FATAL)
	cfg, err := initCfg(c)
	if err != nil {
		return err
	}

	query, err := getQuery(c)
	if err != nil {
		return err
	}

	cli, err := client.NewClient(*cfg.Transport)
	if err != nil {
		return err
	}

	defer cli.Close()
	return shell.Query(newCtx(), query, c.Bool(argQueryStreamMode), cli)
}

//===================== shell =====================

func runShell(c *ucli.Context) error {
	log4g.SetLogLevel("", log4g.FATAL)
	cfg, err := initCfg(c)
	if err != nil {
		return err
	}

	cli, err := client.NewClient(*cfg.Transport)
	if err != nil {
		return err
	}

	defer cli.Close()
	return shell.Run(cli)
}

func getQuery(c *ucli.Context) ([]string, error) {
	var (
		query []string
	)

	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 { //check if NOT file input
		if len(c.Args().Slice()) != 0 {
			query = append(query, strings.Join(c.Args().Slice(), " "))
			return query, nil
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() { //for now just read it all, later pipe if needed
		t := strings.TrimSpace(scanner.Text())
		if t != "" {
			query = append(query, t)
		}
	}

	return query, scanner.Err()
}
