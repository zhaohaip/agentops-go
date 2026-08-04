package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/zhaohaip/agentops-go/internal/app"
	"github.com/zhaohaip/agentops-go/internal/config/infra"
)

const defaultConfigPath = "configs/infra.yaml"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		log.New(os.Stderr, "agentops: ", 0).Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, logOutput io.Writer) error {
	configPath, err := parseConfigPath(args)
	if err != nil {
		return err
	}

	config, err := infra.Load(configPath)
	if err != nil {
		return err
	}

	host, err := app.NewHost(config, logOutput)
	if err != nil {
		return fmt.Errorf("create runtime host: %w", err)
	}
	if err := host.Run(ctx); err != nil {
		return fmt.Errorf("run runtime host: %w", err)
	}

	return nil
}

func parseConfigPath(args []string) (string, error) {
	flags := flag.NewFlagSet("agentops", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", defaultConfigPath, "path to the infrastructure YAML configuration file")
	if err := flags.Parse(args); err != nil {
		return "", fmt.Errorf("parse startup arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return "", errors.New("parse startup arguments: positional arguments are not supported")
	}
	if *configPath == "" {
		return "", errors.New("parse startup arguments: config path is required")
	}

	return *configPath, nil
}
