package v1alpha1

// Configuration struct
type ConfigSpec struct {
	Server struct {
		ListenAddress string `yaml:"listen_address"`
		Config        struct {
			ReadBufferSize int `yaml:"read_buffer_size"`
		} `yaml:"config"`
	} `yaml:"server"`
	Akamai struct {
		Host         string `yaml:"host"`
		ClientSecret string `yaml:"client_secret"`
		ClientToken  string `yaml:"client_token"`
		AccessToken  string `yaml:"access_token"`
	} `yaml:"akamai"`
	PostPurgeRequest struct {
		Enabled   bool              `yaml:"enabled"`
		UserAgent string            `yaml:"user_agent"`
		Headers   map[string]string `yaml:"headers"`
	} `yaml:"post_purge_request"`
	OriginCachePurge struct {
		Enabled               bool     `yaml:"enabled"`
		Endpoints             []string `yaml:"endpoints"`
		Token                 string   `yaml:"token"`
		TimeoutSeconds        int      `yaml:"timeout_seconds"`
		TotalTimeoutSeconds   int      `yaml:"total_timeout_seconds"`
		InsecureSkipTLSVerify bool     `yaml:"insecure_skip_tls_verify"`
	} `yaml:"origin_cache_purge"`
	Logs struct {
		ShowAccessLogs bool `yaml:"show_access_logs"`
		JwtUser        struct {
			Enabled  bool   `yaml:"enabled"`
			Header   string `yaml:"header"`
			JwtField string `yaml:"jwt_field"`
		} `yaml:"jwt_user"`
		AccessLogsFields []string `yaml:"access_logs_fields"`
	} `yaml:"logs"`
}
