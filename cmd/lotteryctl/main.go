// Command lotteryctl is the admin CLI: migrations now; simulation, verification and reconciliation later.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/db"
)

func parse(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("usage: lotteryctl migrate up|down")
	}
	switch args[0] {
	case "migrate":
		if len(args) == 2 && (args[1] == "up" || args[1] == "down") {
			return "migrate " + args[1], nil
		}
		return "", errors.New("usage: lotteryctl migrate up|down")
	default:
		return "", fmt.Errorf("unknown command %q", args[0])
	}
}

func main() {
	cmd, err := parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	switch cmd {
	case "migrate up":
		err = db.MigrateUp(ctx, cfg.DatabaseURL)
	case "migrate down":
		err = db.MigrateDown(ctx, cfg.DatabaseURL)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
