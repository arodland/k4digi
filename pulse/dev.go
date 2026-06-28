package pulse

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/jfreymuth/pulse"
	"github.com/jfreymuth/pulse/proto"
	log "github.com/rs/zerolog/log"
)

type PipeSource struct {
	Index  uint32
	Handle *os.File
	Name   string
}

type PipeSink struct {
	Index  uint32
	Handle *os.File
	Name   string
}

var quoter = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func quote(v string) string { return `"` + quoter.Replace(v) + `"` }

func propList(kv ...string) string {
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+"="+quote(kv[i+1]))
	}
	return strings.Join(parts, " ")
}

// CreatePipeSource loads a module-pipe-source and returns a handle to its named pipe.
func CreatePipeSource(pc *pulse.Client, name, desc string, sampleRate, channels int) (*PipeSource, error) {
	pipePath := "/tmp/k4digi-" + name + ".pipe"

	var resp proto.LoadModuleReply
	err := pc.RawRequest(
		&proto.LoadModule{
			Name: "module-pipe-source",
			Args: propList(
				"source_name", name,
				"file", pipePath,
				"rate", strconv.Itoa(sampleRate),
				"format", "s16le",
				"channels", strconv.Itoa(channels),
				"source_properties", fmt.Sprintf(
					"device.icon_name=\"radio\" device.description='%s' k4digi.pid=%d",
					desc, os.Getpid()),
			),
		},
		&resp,
	)
	if err != nil {
		return nil, fmt.Errorf("load module-pipe-source: %w", err)
	}

	f, err := os.OpenFile(pipePath, os.O_RDWR, 0)
	if err != nil {
		unloadModule(pc, resp.ModuleIndex)
		return nil, fmt.Errorf("open pipe %s: %w", pipePath, err)
	}
	return &PipeSource{Index: resp.ModuleIndex, Handle: f, Name: name}, nil
}

// CreatePipeSink loads a module-pipe-sink and returns a handle to its named pipe.
func CreatePipeSink(pc *pulse.Client, name, desc string, sampleRate, channels int) (*PipeSink, error) {
	pipePath := "/tmp/k4digi-" + name + ".pipe"

	var resp proto.LoadModuleReply
	err := pc.RawRequest(
		&proto.LoadModule{
			Name: "module-pipe-sink",
			Args: propList(
				"sink_name", name,
				"file", pipePath,
				"rate", strconv.Itoa(sampleRate),
				"format", "s16le",
				"channels", strconv.Itoa(channels),
				"use_system_clock_for_timing", "yes",
				"sink_properties", fmt.Sprintf(
					"device.icon_name=\"radio\" device.description='%s' k4digi.pid=%d",
					desc, os.Getpid()),
			),
		},
		&resp,
	)
	if err != nil {
		return nil, fmt.Errorf("load module-pipe-sink: %w", err)
	}

	f, err := os.OpenFile(pipePath, os.O_RDONLY, 0)
	if err != nil {
		unloadModule(pc, resp.ModuleIndex)
		return nil, fmt.Errorf("open pipe %s: %w", pipePath, err)
	}
	return &PipeSink{Index: resp.ModuleIndex, Handle: f, Name: name}, nil
}

func unloadModule(pc *pulse.Client, index uint32) {
	_ = pc.RawRequest(&proto.UnloadModule{ModuleIndex: index}, nil)
}

// Consume creates a self-consumer that reads the source into io.Discard.
// On PipeWire, module-pipe-source does not clock itself when no real consumer is
// attached: audio accumulates in the pipe buffer, causing a latency burst when
// WSJT-X (or any other app) first connects to K4-RX. The self-consumer keeps the
// pipe drained at the real sample clock so the source is always "live".
func (s *PipeSource) Consume(pc *pulse.Client, sampleRate, channels int) (*pulse.RecordStream, error) {
	src, err := pc.SourceByID(s.Name)
	if err != nil {
		return nil, fmt.Errorf("looking up source %q: %w", s.Name, err)
	}

	chanOpt := pulse.RecordMono
	if channels == 2 {
		chanOpt = pulse.RecordStereo
	}

	rec, err := pc.NewRecord(
		pulse.NewWriter(io.Discard, proto.FormatInt16LE),
		pulse.RecordSampleRate(sampleRate),
		chanOpt,
		pulse.RecordSource(src),
		pulse.RecordMediaName("self-consume"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating self-consume stream: %w", err)
	}
	rec.Start()
	return rec, nil
}

// IsPipeWire returns true when the PulseAudio server is actually PipeWire.
// PipeWire needs the self-consumer workaround for module-pipe-source timing.
func IsPipeWire(pc *pulse.Client) (bool, error) {
	var info proto.GetServerInfoReply
	if err := pc.RawRequest(&proto.GetServerInfo{}, &info); err != nil {
		return false, fmt.Errorf("getting PA server info: %w", err)
	}
	log.Debug().Str("pa_server", info.PackageName).Str("pa_version", info.PackageVersion).
		Msg("PulseAudio server info")
	return strings.Contains(info.PackageName, "PipeWire"), nil
}

func (s *PipeSource) Close(pc *pulse.Client) {
	if s.Handle != nil {
		s.Handle.Close()
	}
	unloadModule(pc, s.Index)
}

func (s *PipeSink) Close(pc *pulse.Client) {
	if s.Handle != nil {
		s.Handle.Close()
	}
	unloadModule(pc, s.Index)
}

// CheckConflicts detects stale pipe-source/sink modules from dead k4digi processes
// and unloads them, so we can create new ones cleanly.
func CheckConflicts(pc *pulse.Client, sourceNames []string, sinkName string) error {
	sourceSet := make(map[string]struct{}, len(sourceNames))
	for _, n := range sourceNames {
		sourceSet[n] = struct{}{}
	}

	var srcList proto.GetSourceInfoListReply
	if err := pc.RawRequest(&proto.GetSourceInfoList{}, &srcList); err != nil {
		return fmt.Errorf("listing sources: %w", err)
	}
	for _, src := range srcList {
		if _, ok := sourceSet[src.SourceName]; ok {
			if err := evictStale(pc, src.Properties, src.ModuleIndex, "source", src.SourceName); err != nil {
				return err
			}
		}
	}

	var snkList proto.GetSinkInfoListReply
	if err := pc.RawRequest(&proto.GetSinkInfoList{}, &snkList); err != nil {
		return fmt.Errorf("listing sinks: %w", err)
	}
	for _, snk := range snkList {
		if snk.SinkName == sinkName {
			if err := evictStale(pc, snk.Properties, snk.ModuleIndex, "sink", sinkName); err != nil {
				return err
			}
		}
	}
	return nil
}

func evictStale(pc *pulse.Client, props proto.PropList, moduleIndex uint32, kind, name string) error {
	pidProp, ok := props["k4digi.pid"]
	if !ok {
		return fmt.Errorf("%s %q already exists without a k4digi.pid property", kind, name)
	}
	pid, err := strconv.Atoi(pidProp.String())
	if err != nil {
		return fmt.Errorf("parsing k4digi.pid: %w", err)
	}
	alive, err := processRunning(pid)
	if err != nil {
		return err
	}
	if alive {
		return fmt.Errorf("%s %q is owned by running pid %d", kind, name, pid)
	}
	log.Warn().Int("pid", pid).Uint32("module", moduleIndex).
		Msgf("%s %q was left by dead pid %d, unloading", kind, name, pid)
	return pc.RawRequest(&proto.UnloadModule{ModuleIndex: moduleIndex}, nil)
}

func processRunning(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch err {
	case nil:
		return true, nil
	case syscall.ESRCH:
		return false, nil
	default:
		return false, err
	}
}
