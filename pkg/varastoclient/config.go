package varastoclient

import (
	"fmt"
	"github.com/function61/gokit/fileexists"
	"github.com/function61/gokit/jsonfile"
	gohomedir "github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"io"
	"os"
	"path/filepath"
)

const (
	configFilename = "bup-config.json"
)

type ClientConfig struct {
	ServerAddr string `json:"server_addr"`
	AuthToken  string `json:"auth_token"`
}

func (c *ClientConfig) ApiPath(path string) string {
	return c.ServerAddr + path
}

func writeConfig(conf *ClientConfig) error {
	confPath, err := configFilePath()
	if err != nil {
		return err
	}

	return jsonfile.Write(confPath, conf)
}

func readConfig() (*ClientConfig, error) {
	confPath, err := configFilePath()
	if err != nil {
		return nil, err
	}

	conf := &ClientConfig{}
	if err := jsonfile.Read(confPath, conf, true); err != nil {
		return nil, err
	}

	return conf, nil
}

func configFilePath() (string, error) {
	usersHomeDirectory, err := gohomedir.Dir()
	if err != nil {
		return "", err
	}

	return filepath.Join(usersHomeDirectory, configFilename), nil
}

func configInitEntrypoint() *cobra.Command {
	return &cobra.Command{
		Use:   "config-init [serverAddr] [authToken]",
		Short: "Initialize configuration, use http://localhost:8066 for dev",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			serverAddr := args[0]
			authToken := args[1]

			confPath, err := configFilePath()
			if err != nil {
				panic(err)
			}

			exists, err := fileexists.Exists(confPath)
			if err != nil {
				panic(err)
			}

			if exists {
				panic("config file already exists")
			}

			conf := &ClientConfig{
				ServerAddr: serverAddr,
				AuthToken:  authToken,
			}

			if err := writeConfig(conf); err != nil {
				panic(err)
			}
		},
	}
}

func configPrintEntrypoint() *cobra.Command {
	return &cobra.Command{
		Use:   "config-print",
		Short: "Prints path to config file & its contents",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			confPath, err := configFilePath()
			if err != nil {
				panic(err)
			}

			fmt.Printf("path: %s\n", confPath)

			exists, err := fileexists.Exists(confPath)
			if err != nil {
				panic(err)
			}

			if !exists {
				fmt.Println(".. does not exist. run config-init to fix that")
				return
			}

			file, err := os.Open(confPath)
			if err != nil {
				panic(err)
			}
			defer file.Close()

			if _, err := io.Copy(os.Stdout, file); err != nil {
				panic(err)
			}
		},
	}
}