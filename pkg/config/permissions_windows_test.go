// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func secretFileOwner(t *testing.T, path string) *windows.SID {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	require.NoError(t, err)
	owner, _, err := sd.Owner()
	require.NoError(t, err)
	require.NotNil(t, owner)
	return owner
}

func setSecretFileDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	require.NoError(t, err)
	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil))
}

func setSecretFilePermissions(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	sddl := fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", secretFileOwner(t, path))
	if mode.Perm()&0o007 != 0 {
		sddl += "(A;;FR;;;WD)"
	}
	setSecretFileDACL(t, path, sddl)
}

func TestHasOthersPermissionWindows(t *testing.T) {
	cases := []struct {
		name   string
		ace    string
		others bool
	}{
		{name: "owner only"},
		{name: "system and administrators", ace: "(A;;FA;;;SY)(A;;FA;;;BA)"},
		{name: "everyone can read", ace: "(A;;FR;;;WD)", others: true},
		{name: "users can read", ace: "(A;;FR;;;BU)", others: true},
		{name: "authenticated users can read", ace: "(A;;FR;;;AU)", others: true},
		{name: "everyone can write", ace: "(A;;FW;;;WD)", others: true},
		{name: "everyone can change ACL", ace: "(A;;WD;;;WD)", others: true},
		{name: "everyone can change owner", ace: "(A;;WO;;;WD)", others: true},
		{name: "everyone can delete", ace: "(A;;SD;;;WD)", others: true},
		{name: "metadata only", ace: "(A;;0x00120080;;;WD)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSecretFile(t, "secret")
			setSecretFileDACL(t, path, fmt.Sprintf("D:P(A;;FA;;;%s)%s", secretFileOwner(t, path), tc.ace))
			others, err := HasOthersPermission(path)
			require.NoError(t, err)
			require.Equal(t, tc.others, others)
		})
	}

	t.Run("inherited readers are rejected", func(t *testing.T) {
		dir := t.TempDir()
		setSecretFileDACL(t, dir, fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FR;;;WD)", secretFileOwner(t, dir)))
		path := filepath.Join(dir, "secret")
		require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))
		others, err := HasOthersPermission(path)
		require.NoError(t, err)
		require.True(t, others)
	})

	t.Run("null DACL is rejected", func(t *testing.T) {
		path := writeSecretFile(t, "secret")
		require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil, nil, nil, nil))
		others, err := HasOthersPermission(path)
		require.NoError(t, err)
		require.True(t, others)
	})

	t.Run("missing file returns error", func(t *testing.T) {
		_, err := HasOthersPermission(filepath.Join(t.TempDir(), "missing"))
		require.Error(t, err)
	})
}

func TestTURNSecretFileWindowsPermissions(t *testing.T) {
	path := writeSecretFile(t, "secret")
	setSecretFilePermissions(t, path, 0o604)
	conf := &Config{}
	conf.RTC.TURNServers = []TURNServer{{Host: "h", SecretFile: path}}
	require.ErrorIs(t, conf.LoadTURNSecrets(), ErrTURNSecretFileIncorrectPermission)
	require.Empty(t, conf.RTC.TURNServers[0].Secret)
}

func TestSecretFileWindowsSecurityDescriptor(t *testing.T) {
	path := writeSecretFile(t, "secret")
	owner := secretFileOwner(t, path)
	cases := []struct {
		name      string
		sddl      string
		others    bool
		wantError bool
	}{
		{name: "users group owner", sddl: "O:BUD:P(A;;FA;;;BU)", others: true},
		{name: "everyone owner", sddl: "O:WDD:P(A;;FA;;;WD)", others: true},
		{name: "authenticated users owner", sddl: "O:AUD:P(A;;FA;;;AU)", others: true},
		{name: "missing owner", sddl: "D:P(A;;FA;;;SY)", others: true},
		{name: "missing DACL", sddl: fmt.Sprintf("O:%s", owner), wantError: true},
		{name: "empty DACL", sddl: fmt.Sprintf("O:%sD:P", owner)},
		{name: "object ACE", sddl: fmt.Sprintf("O:%sD:P(OA;;FR;00000000-0000-0000-0000-000000000001;;WD)", owner), wantError: true},
		{name: "inherit-only ACE", sddl: fmt.Sprintf("O:%sD:P(A;IO;FR;;;WD)", owner)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			require.NoError(t, err)
			others, err := secretFileHasOthersPermission(sd)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.others, others)
		})
	}

	t.Run("missing security descriptor", func(t *testing.T) {
		others, err := secretFileHasOthersPermission(nil)
		require.NoError(t, err)
		require.True(t, others)
	})
}
