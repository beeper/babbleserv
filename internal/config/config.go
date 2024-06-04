package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type DatabaseConfig struct {
	ClusterFilePath       string `yaml:"clusterFilePath"`
	TransactionTimeout    int64  `yaml:"transactionTimeout"`
	TransactionRetryLimit int64  `yaml:"transactionRetryLimit"`
}

type ServerConfig struct {
	ListenAddr    string   `yaml:"listenAddr"`
	ServiceGroups []string `yaml:"serviceGroups"`
}

type BabbleConfig struct {
	Servers   []ServerConfig `yaml:"servers"`
	Databases struct {
		Rooms DatabaseConfig `yaml:"rooms"`
	} `yaml:"databases"`
	Rooms struct {
		IDGeneratorSalt []byte `yaml:"idGeneratorSalt"`
	} `yaml:"rooms"`
}

func NewBabbleConfig(filename string) BabbleConfig {
	data, err := os.ReadFile(filename)
	if err != nil {
		panic(err)
	}

	var cfg BabbleConfig
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		panic(err)
	}

	return cfg
}
