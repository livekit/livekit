package serverlogger

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/evalphobia/logrus_sentry"
	"github.com/getsentry/raven-go"
	"github.com/johntdyer/slackrus"
	"github.com/lingdor/stackerror"
	"github.com/sirupsen/logrus"
)

var (
	log  *logrus.Logger
	once sync.Once
)

// PrefixField prefix 필드
type PrefixField struct {
	Service string
}

const localMode = "local"

// Initialize 로그 생성
func Initialize(service ...string) {
	var (
		serviceName string = localMode
		mode        string = localMode
	)

	once.Do(func() {
		log = logrus.New()

		if service != nil {
			serviceName = service[0]

			if len(service) > 1 {
				mode = service[1]
			}
		}

		log.SetFormatter(&formatter{
			serviceName,
			logrus.TextFormatter{
				FullTimestamp:          true,
				TimestampFormat:        "2006-01-02 15:04:05",
				ForceColors:            true,
				DisableLevelTruncation: true,
			},
		})
		log.SetLevel(logrus.DebugLevel)

		if mode != localMode {
			log.SetLevel(logrus.InfoLevel)
			addSlackHook(log, serviceName)
			addSentryHook(log)
		}
	})
}

func addSlackHook(log *logrus.Logger, serviceName string) {
	hookURL := os.Getenv("SLACK_ALRAM_URL")
	channel := "dev-service-alarm"
	acceptedLevels := []logrus.Level{
		logrus.InfoLevel,
		logrus.WarnLevel,
		logrus.ErrorLevel,
		logrus.FatalLevel,
	}

	log.AddHook(&slackrus.SlackrusHook{
		HookURL:        hookURL,
		AcceptedLevels: acceptedLevels,
		Channel:        channel,
		IconEmoji:      ":ghost:",
		Username:       "Ghost-bot",
		Extra: map[string]interface{}{
			"ServiceName": serviceName,
			"Timestamp":   LocalTime().Format(DatetimeLayout),
			// "Level":       log.Level,
		},
	})
}

func addSentryHook(log *logrus.Logger) {
	dsn := os.Getenv("SENTRY_DSN")
	mode := os.Getenv("MODE")

	client, err := raven.New(dsn)
	if err != nil {
		log.Fatal(err)
	}

	client.SetEnvironment(mode)
	hook, err := logrus_sentry.NewWithClientSentryHook(client, []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
	})

	if err == nil {
		log.Hooks.Add(hook)
	}
}

// WithFields 필드정보 추가
func WithFields(fields logrus.Fields) *logrus.Entry {
	return log.WithFields(fields)
}

// GetField 필드정보 추출
func GetField(i interface{}) logrus.Fields {
	b, _ := json.Marshal(i)
	fields := logrus.Fields{}
	json.Unmarshal(b, &fields)
	return fields
}

// Log 로그 추가
func Log(level logrus.Level, message string, args ...interface{}) {
	log.Log(level, args...)
}

// Debug 디버그
func Debug(args ...interface{}) {
	log.Debug(args...)
}

// Info ...
func Info(args ...interface{}) {
	log.Info(args...)
}

// Warn ...
func Warn(args ...interface{}) {
	log.Warn(args...)
}

// Error ...
func Error(args ...interface{}) {
	// log.Error(args...)
	log.Error(stackerror.New(fmt.Sprint(args...)))
}

// Fatal ...
func Fatal(args ...interface{}) {
	// log.Fatal(args...)
	log.Fatal(stackerror.New(fmt.Sprint(args...)))
}

// Logf ...
func Logf(level logrus.Level, message string, args ...interface{}) {
	log.Logf(level, message, args...)
}

// Debugf ...
func Debugf(message string, args ...interface{}) {
	log.Debugf(message, args...)
}

// Infof ...
func Infof(message string, args ...interface{}) {
	log.Infof(message, args...)
}

// Warnf ...
func Warnf(message string, args ...interface{}) {
	log.Warnf(message, args...)
}

// Errorf ...
func Errorf(message string, args ...interface{}) {
	log.Errorf(message, args...)
}

// Fatalf ...
func Fatalf(message string, args ...interface{}) {
	log.Fatalf(message, args...)
}
