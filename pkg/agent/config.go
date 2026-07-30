package agent

const DefaultTargetLoad = 0.7

type Config struct {
	EnableUserDataRecording bool    `yaml:"enable_user_data_recording"`
	EnableUserDataRedaction bool    `yaml:"enable_user_data_redaction"`
	TargetLoad              float32 `yaml:"target_load,omitempty"`
}
