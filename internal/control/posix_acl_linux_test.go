package control

import (
	"io/fs"
	"testing"
)

func TestMergeAccessACLPreservesExistingNamedEntries(t *testing.T) {
	t.Parallel()

	const existingUserID uint32 = 2000
	const controlGroupID uint32 = 3000
	existing := []aclEntry{
		{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
		{Tag: aclUser, Permissions: aclRead, ID: existingUserID},
		{Tag: aclGroupObject, Permissions: 0, ID: aclUndefinedID},
		{Tag: aclMask, Permissions: aclRead, ID: aclUndefinedID},
		{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
	}

	merged, err := mergeAccessACL(existing, fs.FileMode(0o740), []aclEntry{
		{Tag: aclGroup, Permissions: aclExecute, ID: controlGroupID},
	})
	if err != nil {
		t.Fatalf("mergeAccessACL: %v", err)
	}
	requireACLEntry(t, merged, aclUser, existingUserID, aclRead)
	requireACLEntry(t, merged, aclGroup, controlGroupID, aclExecute)
	requireACLEntry(t, merged, aclMask, aclUndefinedID, aclRead|aclExecute)
}

func TestMergeAccessACLDoesNotBroadenMaskedPermissions(t *testing.T) {
	t.Parallel()

	const existingUserID uint32 = 2000
	const controlGroupID uint32 = 3000
	existing := []aclEntry{
		{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
		{Tag: aclUser, Permissions: aclRead | aclExecute, ID: existingUserID},
		{Tag: aclGroupObject, Permissions: aclRead | aclExecute, ID: aclUndefinedID},
		{Tag: aclMask, Permissions: aclRead, ID: aclUndefinedID},
		{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
	}

	merged, err := mergeAccessACL(existing, fs.FileMode(0o740), []aclEntry{
		{Tag: aclGroup, Permissions: aclExecute, ID: controlGroupID},
	})
	if err != nil {
		t.Fatalf("mergeAccessACL: %v", err)
	}
	requireACLEntry(t, merged, aclGroup, controlGroupID, aclExecute)
	requireACLEntry(t, merged, aclMask, aclUndefinedID, aclRead|aclExecute)
	requireACLEntry(t, merged, aclUser, existingUserID, aclRead)
	requireACLEntry(t, merged, aclGroupObject, aclUndefinedID, aclRead)
	if permissions, _ := aclPermissions(merged, aclUser, existingUserID); permissions&aclExecute != 0 {
		t.Errorf("existing user permissions = %#o, unexpectedly gained execute", permissions)
	}
	if permissions, _ := aclPermissions(merged, aclGroupObject, aclUndefinedID); permissions&aclExecute != 0 {
		t.Errorf("owning group permissions = %#o, unexpectedly gained execute", permissions)
	}
}

func TestACLGuaranteesGroupAccessUsesGroupObjectInsteadOfModeMask(t *testing.T) {
	t.Parallel()

	const controlGroupID uint32 = 3000
	entries := []aclEntry{
		{Tag: aclUserObject, Permissions: aclRead | aclWrite | aclExecute, ID: aclUndefinedID},
		{Tag: aclUser, Permissions: aclExecute, ID: 2000},
		{Tag: aclGroupObject, Permissions: 0, ID: aclUndefinedID},
		{Tag: aclMask, Permissions: aclExecute, ID: aclUndefinedID},
		{Tag: aclOther, Permissions: 0, ID: aclUndefinedID},
	}
	if aclGuaranteesGroupAccess(
		entries,
		fs.FileMode(0o710),
		controlGroupID,
		controlGroupID,
		aclExecute,
	) {
		t.Fatal("group access succeeded from ACL mask without owning-group permission")
	}

	entries = append(entries, aclEntry{
		Tag:         aclGroup,
		Permissions: aclExecute,
		ID:          controlGroupID,
	})
	if !aclGuaranteesGroupAccess(
		entries,
		fs.FileMode(0o710),
		controlGroupID,
		controlGroupID,
		aclExecute,
	) {
		t.Fatal("named control-group access was not recognized")
	}
}
