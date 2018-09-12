package integration

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type Sandbox interface {
	Address() string
	PrintLogs(*testing.T)
	Cmd(...string) *exec.Cmd
	NewRegistry() (string, error)
	Rootless() bool
	Value(string) interface{} // chosen matrix value
}

type Worker interface {
	New(...SandboxOpt) (Sandbox, func() error, error)
	Name() string
}

type SandboxConf struct {
	mirror string
	mv     matrixValue
}

type SandboxOpt func(*SandboxConf)

func WithMirror(h string) SandboxOpt {
	return func(c *SandboxConf) {
		c.mirror = h
	}
}

func withMatrixValues(mv matrixValue) SandboxOpt {
	return func(c *SandboxConf) {
		c.mv = mv
	}
}

type Test func(*testing.T, Sandbox)

var defaultWorkers []Worker

func register(w Worker) {
	defaultWorkers = append(defaultWorkers, w)
}

func List() []Worker {
	return defaultWorkers
}

type TestOpt func(*TestConf)

func WithMatrix(key string, m map[string]interface{}) TestOpt {
	return func(tc *TestConf) {
		if tc.matrix == nil {
			tc.matrix = map[string]map[string]interface{}{}
		}
		tc.matrix[key] = m
	}
}

type TestConf struct {
	matrix map[string]map[string]interface{}
}

func Run(t *testing.T, testCases []Test, opt ...TestOpt) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	var tc TestConf
	for _, o := range opt {
		o(&tc)
	}

	mirror, cleanup, err := runMirror(t)
	require.NoError(t, err)

	var mu sync.Mutex
	var count int
	cleanOnComplete := func() func() {
		count++
		return func() {
			mu.Lock()
			count--
			if count == 0 {
				cleanup()
			}
			mu.Unlock()
		}
	}
	defer cleanOnComplete()()

	matrix := prepareValueMatrix(tc)

	for _, br := range List() {
		for _, tc := range testCases {
			for _, mv := range matrix {
				ok := t.Run(getFunctionName(tc)+"/worker="+br.Name()+mv.functionSuffix(), func(t *testing.T) {
					defer cleanOnComplete()()
					sb, close, err := br.New(WithMirror(mirror), withMatrixValues(mv))
					if err != nil {
						if errors.Cause(err) == ErrorRequirements {
							t.Skip(err.Error())
						}
						require.NoError(t, err)
					}
					defer func() {
						assert.NoError(t, close())
						if t.Failed() {
							sb.PrintLogs(t)
						}
					}()
					tc(t, sb)
				})
				require.True(t, ok)
			}
		}
	}
}

func getFunctionName(i interface{}) string {
	fullname := runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	dot := strings.LastIndex(fullname, ".") + 1
	return strings.Title(fullname[dot:])
}

func copyImagesLocal(t *testing.T, host string) error {
	for to, from := range offlineImages() {
		desc, provider, err := contentutil.ProviderFromRef(from)
		if err != nil {
			return err
		}
		ingester, err := contentutil.IngesterFromRef(host + "/" + to)
		if err != nil {
			return err
		}
		if err := contentutil.CopyChain(context.TODO(), ingester, provider, desc); err != nil {
			return err
		}
		t.Logf("copied %s to local mirror %s", from, host+"/"+to)
	}
	return nil
}

func offlineImages() map[string]string {
	arch := runtime.GOARCH
	if arch == "arm64" {
		arch = "arm64v8"
	}
	return map[string]string{
		"library/busybox:latest": "docker.io/" + arch + "/busybox:latest",
		"library/alpine:latest":  "docker.io/" + arch + "/alpine:latest",
		"tonistiigi/copy:v0.1.4": "docker.io/" + dockerfile2llb.DefaultCopyImage,
	}
}

func configWithMirror(mirror string) (string, error) {
	tmpdir, err := ioutil.TempDir("", "bktest_config")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(tmpdir, 0711); err != nil {
		return "", err
	}
	if err := ioutil.WriteFile(filepath.Join(tmpdir, "buildkitd.toml"), []byte(fmt.Sprintf(`
[registry."docker.io"]
mirrors=["%s"]
`, mirror)), 0644); err != nil {
		return "", err
	}
	return tmpdir, nil
}

func runMirror(t *testing.T) (host string, cleanup func() error, err error) {
	mirrorDir := os.Getenv("BUILDKIT_REGISTRY_MIRROR_DIR")

	var f *os.File
	if mirrorDir != "" {
		f, err = os.Create(filepath.Join(mirrorDir, "lock"))
		if err != nil {
			return "", nil, err
		}
		defer func() {
			if err != nil {
				f.Close()
			}
		}()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			return "", nil, err
		}
	}

	mirror, cleanup, err := newRegistry(mirrorDir)
	if err != nil {
		return "", nil, err
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	if err := copyImagesLocal(t, mirror); err != nil {
		return "", nil, err
	}

	if mirrorDir != "" {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			return "", nil, err
		}
	}

	return mirror, cleanup, err
}

type matrixValue struct {
	fn     []string
	values map[string]matrixValueChoice
}

func (mv matrixValue) functionSuffix() string {
	if len(mv.fn) == 0 {
		return ""
	}
	sort.Strings(mv.fn)
	sb := &strings.Builder{}
	for _, f := range mv.fn {
		sb.Write([]byte("/" + f + "=" + mv.values[f].name))
	}
	return sb.String()
}

type matrixValueChoice struct {
	name  string
	value interface{}
}

func newMatrixValue(key, name string, v interface{}) matrixValue {
	return matrixValue{
		fn: []string{key},
		values: map[string]matrixValueChoice{
			key: matrixValueChoice{
				name:  name,
				value: v,
			},
		},
	}
}

func prepareValueMatrix(tc TestConf) []matrixValue {
	m := []matrixValue{}
	for featureName, values := range tc.matrix {
		current := m
		m = []matrixValue{}
		for featureValue, v := range values {
			if len(current) == 0 {
				m = append(m, newMatrixValue(featureName, featureValue, v))
			}
			for _, c := range current {
				vv := newMatrixValue(featureName, featureValue, v)
				vv.fn = append(vv.fn, c.fn...)
				for k, v := range c.values {
					vv.values[k] = v
				}
				m = append(m, vv)
			}
		}
	}
	if len(m) == 0 {
		m = append(m, matrixValue{})
	}
	return m
}
