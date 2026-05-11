package config

import (
	importRand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Protocol string `yaml:"protocol"`
	Port     string `yaml:"port"`
	Dest     string `yaml:"dest"`
	Debug    bool   `yaml:"debug"`

	Xtls struct {
		PrivateKey  string `yaml:"private-key"`
		ShortId     string `yaml:"short-id"`
		ServerNames string `yaml:"server-names"`
	} `yaml:"xtls"`

	Hysteria struct {
		QuicPort     string `yaml:"quic-port"`
		AuthKey      string `yaml:"auth-key"`
		AuthPassword string `yaml:"auth-password"`
	} `yaml:"hysteria"`

	Torrent struct {
		AuthKey  string `yaml:"auth-key"`
		InfoHash string `yaml:"info-hash"`
	} `yaml:"torrent"`
}

func LoadConfig(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return createDefaultConfig(path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Protocol: "xtls",
		Port:     "443",
		Dest:     "samsung.com:443",
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func createDefaultConfig(path string) (*Config, error) {
	// Генерация ключей для дефолтного конфига
	privKey := make([]byte, 32)
	importRand.Read(privKey)
	
	shortId := make([]byte, 8)
	importRand.Read(shortId)

	cfg := &Config{
		Protocol: "xtls",
		Port:     "443",
		Dest:     "samsung.com:443",
	}
	cfg.Xtls.PrivateKey = base64.StdEncoding.EncodeToString(privKey)
	cfg.Xtls.ShortId = hex.EncodeToString(shortId)

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return nil, err
	}

	return cfg, nil
}