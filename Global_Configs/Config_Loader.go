package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

type App_Config struct {
	Server struct {
		ServiceAPort int `yaml:"service_a_port"`
		ServiceBPort int `yaml:"service_b_port"`
		MetricsPort  int `yaml:"metrics_port"`
	} `yaml:"server"`

	Workers struct {
		MaxPythonWorkers int `yaml:"max_python_workers"`
	} `yaml:"workers"`

	Paths struct {
		AzurePath  string `yaml:"azure_path"`
		ModelCache string `yaml:"model_cache"`
	} `yaml:"paths"`

	Drift struct {
		Threshold float64 `yaml:"threshold"`
	} `yaml:"drift"`

	Connections struct {
		DuckDBPath         string `yaml:"duckdb_path"`
		RedisAddr          string `yaml:"redis_addr"`
		AzureConn          string `yaml:"azure_conn"`
		AzureContainerName string `yaml:"azure_container_name"`
	} `yaml:"connections"`
}

// load azure info and app.yaml info into the config struct
func LoadConfig() (*App_Config, error) {
	app_config_struct := &App_Config{}

	f, err := os.Open("Global_Configs/App.yaml")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// this loads in the app.yaml info into the struct. it knows where to put the data because of the yaml tags in the struct
	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(app_config_struct); err != nil {
		return nil, err
	}

	// loads into system env variables for this process
	if err := godotenv.Load("Global_Configs/Env/Azure.env"); err != nil {
		log.Printf("WARNING: Global_Configs/Env/Azure.env file not found or could not be loaded: %v", err)
	}

	if conn := os.Getenv("AZURE_STORAGE_CONNECTION_STRING"); conn != "" {
		app_config_struct.Connections.AzureConn = conn
	}
	if container := os.Getenv("AZURE_CONTAINER_NAME"); container != "" {
		app_config_struct.Connections.AzureContainerName = container
	}

	return app_config_struct, nil
}
