package main

import (
	"flag"
	"fmt"
	"log"

	"spo-data/configs"
	"spo-data/internal/balance"
	"spo-data/internal/blocks"
	"spo-data/internal/database"

	"github.com/blockfrost/blockfrost-go"
)

func main() {

	var (
		err    error
		config configs.Config
	)

	balanceCmd := flag.NewFlagSet("balance", flag.ExitOnError)
	blockNumCmd := flag.NewFlagSet("blocks", flag.ExitOnError)
	unclaimedCmd := flag.NewFlagSet("unclaimed-rewards", flag.ExitOnError)
	historyCmd := flag.NewFlagSet("rewards-history", flag.ExitOnError)

	configFile := flag.String("config", "example.config.yaml", "Path to yaml config file")
	flag.Parse()

	config, err = configs.LoadConfigFromYAML(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config file: %v", err)
	}

	if len(flag.Args()) < 1 {
		fmt.Println("Expected a subcommand (e.g., 'balance', 'blocks', 'unclaimed-rewards', 'rewards-history')")
		fmt.Println("Available flags:")
		flag.PrintDefaults()
		return
	}

	err = database.Initialize("./data.db")

	if err != nil {
		log.Fatal("Failed to initialize database:", err)
	}

	blockfrostClient := blockfrost.NewAPIClient(
		blockfrost.APIClientOptions{
			Server: config.BlockFrostAddress,
		},
	)

	switch flag.Args()[0] {
	case "balance":
		balanceCmd.Parse(flag.Args()[1:])
		_, err := balance.CalculateBalance(&config, blockfrostClient)
		if err != nil {
			log.Fatalf("Failed to calculate balance: %v", err)
		}
	case "blocks":
		blockNumCmd.Parse(flag.Args()[1:])
		for _, pool := range config.Pools {
			blockNum := blocks.GetPoolBlocksForEpoch(pool.PoolID, blockfrostClient)
			if err != nil {
				log.Fatalf("Failed to get pool blocks for epoch: %v", err)
			}
			fmt.Println("Pool:", pool.Name, "Block Count:", blockNum)
		}
	case "unclaimed-rewards":
		unclaimedCmd.Parse(flag.Args()[1:])
		_, err := balance.GetUnclaimedRewards(&config, blockfrostClient)
		if err != nil {
			log.Fatalf("Failed to get unclaimed rewards: %v", err)
		}

	case "rewards-history":
		historyCmd.Parse(flag.Args()[1:])
		_, err := balance.GetRewardsHistory(&config, blockfrostClient)
		if err != nil {
			log.Fatalf("Failed to get rewards history: %v", err)
		}
	}

}
