package main

import (
	"flag"
	"log"

	"umbrella_server/internal/config"
	"umbrella_server/internal/hysteria"
	"umbrella_server/internal/torrent"
	"umbrella_server/internal/xtls"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("[ERR] Failed to load config from %s: %v", *configPath, err)
	}

	switch cfg.Protocol {
	case "xtls":
		xtls.XtlsStarter(cfg)
	case "hysteria":
		hysteria.HysteriaStarter(cfg)
	case "torrent":
		torrent.TorrentStarter(cfg)
	default:
		log.Fatalf("[ERR] Not valid protocol in config")
	}
}
