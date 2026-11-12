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
	"unsafe"

	"golang.org/x/sys/windows"
)

// HasOthersPermission reports whether a secret file grants data access or
// control to anyone other than its owner, SYSTEM, or Administrators. Windows
// mode bits do not describe file access, so inspect the DACL, including inherited
// permissions. Reading metadata alone does not expose the secret.
func HasOthersPermission(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, fmt.Errorf("reading secret file permissions: %w", err)
	}
	return secretFileHasOthersPermission(sd)
}

func secretFileHasOthersPermission(sd *windows.SECURITY_DESCRIPTOR) (bool, error) {
	if sd == nil {
		return true, nil
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, fmt.Errorf("reading secret file owner: %w", err)
	}
	if owner == nil {
		return true, nil
	}
	trustedOwner := owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid)
	if !trustedOwner {
		_, _, accountType, err := owner.LookupAccount("")
		if err != nil {
			return false, fmt.Errorf("looking up secret file owner: %w", err)
		}
		// An arbitrary group owner must not make that group's members trusted.
		// Owners can change the DACL even without an explicit allow ACE.
		if accountType != windows.SidTypeUser && accountType != windows.SidTypeComputer {
			return true, nil
		}
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, fmt.Errorf("reading secret file DACL: %w", err)
	}
	// A null DACL allows everyone full access; an empty DACL allows no access.
	if dacl == nil {
		return true, nil
	}
	const sensitiveAccess = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
		windows.FILE_EXECUTE | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, fmt.Errorf("reading secret file ACE: %w", err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			// Conservatively inspect grants without trying to resolve group
			// membership or cancel grants against preceding deny entries.
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Mask&sensitiveAccess == 0 {
				continue
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.Equals(owner) && !sid.IsWellKnown(windows.WinLocalSystemSid) &&
				!sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
				return true, nil
			}
		default:
			return false, fmt.Errorf("unsupported secret file ACE type %d", ace.Header.AceType)
		}
	}
	return false, nil
}
