package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPluginEvidenceDirRelativeDeploymentRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("DATA_DIR", "")
	for _, configured := range []string{"", "custom/plugins", filepath.Join(t.TempDir(), "plugins")} {
		cfg := &config.Config{}
		cfg.Plugins.DataDir = configured
		root := resolvePluginRootDir(cfg)
		dir, err := pluginEvidenceDir(filepath.Join(root, "runtime"), "lyunlong.codex.lb-cookie-pin")
		require.NoError(t, err)
		require.True(t, filepath.IsAbs(dir), "plugin requires an absolute evidence path: %s", dir)
		expected, err := filepath.Abs(filepath.Join(root, "state"))
		require.NoError(t, err)
		require.Equal(t, expected, filepath.Dir(dir))
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	}
}
