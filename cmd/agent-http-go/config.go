package main

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "config.yaml"
	defaultHost       = "127.0.0.1"
	defaultPort       = "8787"
)

// Config 表示命令入口运行 HTTP 服务所需的配置。
type Config struct {
	Host string
	Port string
}

// ConfigOptions 控制配置文件路径和环境变量来源。
// Env 可注入，测试时不用修改真实进程环境也能验证环境变量覆盖逻辑。
type ConfigOptions struct {
	Path string
	Env  map[string]string
}

// configFile 对应 YAML 文件的顶层结构。
type configFile struct {
	Server serverConfig `yaml:"server"`
}

// serverConfig 对应 YAML 中的 server 配置段。
type serverConfig struct {
	Host string `yaml:"host"`
	Port string `yaml:"port"`
}

// LoadConfig 按默认值、YAML 文件、环境变量的顺序加载运行配置。
func LoadConfig(options ConfigOptions) (Config, error) {
	config := Config{
		Host: defaultHost,
		Port: defaultPort,
	}

	path := options.Path
	if path == "" {
		path = defaultConfigPath
	}

	fileConfig, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}
	if fileConfig.Server.Host != "" {
		config.Host = fileConfig.Server.Host
	}
	if fileConfig.Server.Port != "" {
		config.Port = fileConfig.Server.Port
	}

	// 环境变量放在最后覆盖，部署时可以不改配置文件就临时调整监听地址。
	env := options.Env
	if env == nil {
		env = environMap()
	}
	if value := env["HOST"]; value != "" {
		config.Host = value
	}
	if value := env["PORT"]; value != "" {
		config.Port = value
	}

	return config, nil
}

// readConfigFile 读取 YAML 配置文件；文件不存在时使用默认配置继续启动。
func readConfigFile(path string) (configFile, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configFile{}, nil
		}
		return configFile{}, err
	}

	var config configFile
	if err := yaml.Unmarshal(payload, &config); err != nil {
		return configFile{}, err
	}
	return config, nil
}

// environMap 将当前进程环境变量转换成便于覆盖配置的 map。
func environMap() map[string]string {
	env := make(map[string]string)
	for _, item := range os.Environ() {
		for i, char := range item {
			if char == '=' {
				env[item[:i]] = item[i+1:]
				break
			}
		}
	}
	return env
}
