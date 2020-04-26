package main

import (
	"flag"
	"fmt"
	"github.com/pefish/anywherectl/client"
	"github.com/pefish/anywherectl/internal/version"
	"github.com/pefish/anywherectl/listener"
	"github.com/pefish/anywherectl/server"
	go_config "github.com/pefish/go-config"
	go_logger "github.com/pefish/go-logger"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	var subServer SubServerInterface
	secondArg := os.Args[1]
	if secondArg == "serve" {
		subServer = server.NewServer()
	} else if secondArg == "listen" {
		subServer = listener.NewListener(`test`)
	} else {
		subServer = client.NewClient()
	}

	flagSet := flag.NewFlagSet(version.AppName, flag.ExitOnError)

	flagSet.Bool("version", false, "print version string")
	flagSet.String("log-level", "info", "set log verbosity: debug, info, warn, or error")
	flagSet.String("config", "", "path to config file")

	subServer.DecorateFlagSet(flagSet)
	subServer.ParseFlagSet(flagSet)

	configFile := flagSet.Lookup("config").Value.(flag.Getter).Get().(string)
	err := go_config.Config.LoadYamlConfig(go_config.Configuration{
		ConfigFilepath: configFile,
	})
	if err != nil {
		log.Fatal(fmt.Errorf("load config file error - %s", err))
	}
	go_config.Config.MergeFlagSet(flagSet)

	logLevel, err := go_config.Config.GetString("log-level")
	if err != nil {
		log.Fatal(err)
	}
	go_logger.Logger = go_logger.NewLogger(go_logger.WithLevel(logLevel), go_logger.WithIsDebug(true))

	printVersion, err := go_config.Config.GetBool("version")
	if err != nil {
		log.Fatal(err)
	}
	if printVersion {
		go_logger.Logger.Info(version.GetAppVersion(version.AppName))
		os.Exit(0)
	}

	finishChan := make(chan bool, 1)
	subServer.Start(finishChan, flagSet)

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signalChan:
	case <-finishChan:
	}

	go_logger.Logger.Info("Stopping...")
	subServer.Clear()
	go_logger.Logger.Info("Stopped")

}

type SubServerInterface interface {
	DecorateFlagSet(flagSet *flag.FlagSet)
	ParseFlagSet(flagset *flag.FlagSet)
	Start(finishChan chan <- bool, flagSet *flag.FlagSet)
	Exit()
	Clear()
}
