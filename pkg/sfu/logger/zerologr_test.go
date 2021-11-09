package logger

import (
	"bytes"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultVerbosityLevel(t *testing.T) {

	t.Run("info", func(t *testing.T) {
		SetGlobalOptions(GlobalConfig{V: 0})
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.Info("info")
		assert.Equal(t, `{"level":"info","v":0,"time":"NOTIME","message":"info"}`+"\n", out.String())
	})

	t.Run("info-add-fields", func(t *testing.T) {
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.Info("", "test_field", 123)
		assert.Equal(t, `{"level":"info","v":0,"test_field":123,"time":"NOTIME"}`+"\n", out.String())
	})

	t.Run("empty", func(t *testing.T) {
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.Info("")
		assert.Equal(t, `{"level":"info","v":0,"time":"NOTIME"}`+"\n", out.String())
	})

	t.Run("disabled", func(t *testing.T) {
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.V(1).Info("You should not see this")
		assert.Equal(t, ``, out.String())
	})
}

func TestVerbosityLevel2(t *testing.T) {

	t.Run("info", func(t *testing.T) {
		SetGlobalOptions(GlobalConfig{V: 2})
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.Info("info")
		assert.Equal(t, `{"level":"info","v":0,"time":"NOTIME","message":"info"}`+"\n", out.String())
	})

	t.Run("info-add-fields", func(t *testing.T) {
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.Info("", "test_field", 123)
		assert.Equal(t, `{"level":"info","v":0,"test_field":123,"time":"NOTIME"}`+"\n", out.String())
	})

	t.Run("empty", func(t *testing.T) {
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.Info("")
		assert.Equal(t, `{"level":"info","v":0,"time":"NOTIME"}`+"\n", out.String())
	})

	t.Run("disabled", func(t *testing.T) {
		out := &bytes.Buffer{}
		log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: out})
		log.V(1).Info("")
		assert.Equal(t, `{"level":"debug","v":1,"time":"NOTIME"}`+"\n", out.String())
	})
}

type blackholeStream struct {
	writeCount uint64
}

func (s *blackholeStream) WriteCount() uint64 {
	return atomic.LoadUint64(&s.writeCount)
}

func (s *blackholeStream) Write(p []byte) (int, error) {
	atomic.AddUint64(&s.writeCount, 1)
	return len(p), nil
}

func BenchmarkLoggerLogs(b *testing.B) {
	stream := &blackholeStream{}
	log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: stream})
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			log.Info("The quick brown fox jumps over the lazy dog")
		}
	})

	if stream.WriteCount() != uint64(b.N) {
		b.Fatalf("Log write count")
	}
}

func BenchmarLoggerLog(b *testing.B) {
	stream := &blackholeStream{}
	log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: stream})
	b.ResetTimer()
	SetGlobalOptions(GlobalConfig{V: 1})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			log.V(1).Info("The quick brown fox jumps over the lazy dog")
		}
	})

	if stream.WriteCount() != uint64(b.N) {
		b.Fatalf("Log write count")
	}
}

func BenchmarkLoggerLogWith10Fields(b *testing.B) {
	stream := &blackholeStream{}
	log := NewWithOptions(Options{TimeFormat: "NOTIME", Output: stream})
	b.ResetTimer()
	SetGlobalOptions(GlobalConfig{V: 1})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			log.V(1).Info("The quick brown fox jumps over the lazy dog",
				"test1", 1,
				"test2", 2,
				"test3", 3,
				"test4", 4,
				"test5", 5,
				"test6", 6,
				"test7", 7,
				"test8", 8,
				"test9", 9,
				"test10", 10)
		}
	})

	if stream.WriteCount() != uint64(b.N) {
		b.Fatalf("Log write count")
	}
}
