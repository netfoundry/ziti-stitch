/*
	Copyright 2026 NetFoundry Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package config

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// privateDirSDDL grants full control to SYSTEM (the service account) and
// Administrators only, and is protected: nothing is inherited from the
// parent (ProgramData lets every user read). The Owner Rights entry takes
// away the implicit right of an object's owner to change its ACL, so that
// owning a file in it gives no access of its own.
const privateDirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;RC;;;OW)"

// securePrivateDir replaces path's ACL with privateDirSDDL. Files created in
// it afterwards inherit it. File mode bits do not protect anything on
// Windows, so this is what keeps identity keys private there.
//
// It refuses a reparse point, and a directory owned by anyone but SYSTEM,
// an administrator or this process's user. Users may create folders in
// ProgramData: one who made the config directory before the first install
// would own it, and whatever they had put in it (a session key, an
// identity) would stay theirs. Since Windows makes an administrator's own
// account the owner of what they create, an administrator's is trusted,
// and Administrators is made the owner instead, when that is allowed.
func securePrivateDir(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a link to another folder; it must be a folder of its own", path)
	}
	if err := checkDirOwner(path); err != nil {
		return err
	}

	sd, err := windows.SecurityDescriptorFromString(privateDirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	const daclInfo = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	// Becoming the owner needs Administrators to be an owner group in this
	// process's token, so it is not always allowed. The owner checked above
	// is trusted anyway.
	if windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|daclInfo, admins, nil, dacl, nil) == nil {
		return nil
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, daclInfo, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("restricting access to %s: %w", path, err)
	}
	return nil
}

// checkDirOwner reports an error unless path is owned by SYSTEM,
// Administrators, this process's user or an administrator.
func checkDirOwner(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("reading the owner of %s: %w", path, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if trustedOwner(owner, user.User.Sid) {
		return nil
	}
	admin, err := isAdministrator(owner)
	if admin {
		return nil
	}
	name := owner.String()
	if account, domain, _, err := owner.LookupAccount(""); err == nil {
		name = domain + `\` + account
	}
	if err != nil {
		return fmt.Errorf("%s is owned by %s, and whether that is an administrator could not be checked (%v); take ownership of it as an administrator, and try again", path, name, err)
	}
	return fmt.Errorf("%s is owned by %s, not by an administrator; delete it, or take ownership of it as an administrator, and try again", path, name)
}

// trustedOwner reports whether owner is one a process running as self can
// trust without looking further.
func trustedOwner(owner, self *windows.SID) bool {
	return owner.IsWellKnown(windows.WinLocalSystemSid) ||
		owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
		owner.Equals(self)
}

var procNetUserGetLocalGroups = windows.NewLazySystemDLL("netapi32.dll").NewProc("NetUserGetLocalGroups")

const (
	// lgIncludeIndirect asks NetUserGetLocalGroups for the groups an
	// account is in through a domain group too.
	lgIncludeIndirect = 1
	// maxPreferredLength lets it allocate as much as the answer needs.
	maxPreferredLength = 0xFFFFFFFF
	// nerrUserNotFound is its answer for an account that is not a user:
	// a group, say.
	nerrUserNotFound = 2221
)

// isAdministrator reports whether the account sid is in the local
// Administrators group, directly or through a domain group.
func isAdministrator(sid *windows.SID) (bool, error) {
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return false, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	adminsName, _, _, err := admins.LookupAccount("") // localized: "Administrateurs", say
	if err != nil {
		return false, err
	}
	user, err := windows.UTF16PtrFromString(domain + `\` + account)
	if err != nil {
		return false, err
	}
	var buf *byte
	var read, total uint32
	r, _, _ := procNetUserGetLocalGroups.Call(0, uintptr(unsafe.Pointer(user)), 0, lgIncludeIndirect,
		uintptr(unsafe.Pointer(&buf)), maxPreferredLength,
		uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)))
	if buf != nil {
		defer windows.NetApiBufferFree(buf)
	}
	switch r {
	case 0:
	case nerrUserNotFound:
		return false, nil
	default:
		return false, windows.Errno(r)
	}
	// An array of LOCALGROUP_USERS_INFO_0, each a pointer to a group name.
	for _, name := range unsafe.Slice((**uint16)(unsafe.Pointer(buf)), read) {
		if strings.EqualFold(windows.UTF16PtrToString(name), adminsName) {
			return true, nil
		}
	}
	return false, nil
}
