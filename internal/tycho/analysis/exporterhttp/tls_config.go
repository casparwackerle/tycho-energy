package exporterhttp

import (
	"os"

	"gopkg.in/yaml.v3"
)

type WebConfig struct {
	TLSConfig struct {
		CertFile string `yaml:"cert_file"`
		KeyFile  string `yaml:"key_file"`
	} `yaml:"tls_server_config"`
}

// LoadTLSFromWebConfig loads Prometheus-style web.config.file (subset).
func LoadTLSFromWebConfig(path string) (*TLSConfig, error) {
	if path == "" {
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var wc WebConfig
	dec := yaml.NewDecoder(f)
	if err := dec.Decode(&wc); err != nil {
		return nil, err
	}

	if wc.TLSConfig.CertFile == "" || wc.TLSConfig.KeyFile == "" {
		return nil, nil
	}
	return &TLSConfig{CertFile: wc.TLSConfig.CertFile, KeyFile: wc.TLSConfig.KeyFile}, nil
}
