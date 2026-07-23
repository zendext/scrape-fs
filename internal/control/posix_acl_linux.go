package control

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"golang.org/x/sys/unix"
)

const (
	aclAccessXattr = "system.posix_acl_access"
	aclVersion     = 0x0002
	aclUndefinedID = ^uint32(0)

	aclUserObject  uint16 = 0x01
	aclUser        uint16 = 0x02
	aclGroupObject uint16 = 0x04
	aclGroup       uint16 = 0x08
	aclMask        uint16 = 0x10
	aclOther       uint16 = 0x20

	aclRead    uint16 = 0x04
	aclWrite   uint16 = 0x02
	aclExecute uint16 = 0x01
)

type aclEntry struct {
	Tag         uint16
	Permissions uint16
	ID          uint32
}

func readAccessACL(path string) ([]aclEntry, error) {
	size, err := unix.Getxattr(path, aclAccessXattr, nil)
	if errors.Is(err, unix.ENODATA) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read access ACL size for %q: %w", path, err)
	}
	data := make([]byte, size)
	for {
		read, err := unix.Getxattr(path, aclAccessXattr, data)
		if errors.Is(err, unix.ERANGE) {
			size, err = unix.Getxattr(path, aclAccessXattr, nil)
			if err != nil {
				return nil, fmt.Errorf("reread access ACL size for %q: %w", path, err)
			}
			data = make([]byte, size)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read access ACL for %q: %w", path, err)
		}
		return decodeAccessACL(data[:read])
	}
}

func updateAccessACL(
	path string,
	mode fs.FileMode,
	grants []aclEntry,
) error {
	entries, err := readAccessACL(path)
	if err != nil {
		return err
	}
	merged, err := mergeAccessACL(entries, mode, grants)
	if err != nil {
		return fmt.Errorf("merge access ACL for %q: %w", path, err)
	}
	if err := unix.Setxattr(path, aclAccessXattr, encodeAccessACL(merged), 0); err != nil {
		return fmt.Errorf("write access ACL for %q: %w", path, err)
	}
	installed, err := readAccessACL(path)
	if err != nil {
		return err
	}
	for _, grant := range grants {
		if !aclContains(installed, grant) {
			return fmt.Errorf(
				"verify access ACL for %q: missing tag=%d id=%d permissions=%#o",
				path,
				grant.Tag,
				grant.ID,
				grant.Permissions,
			)
		}
	}
	return nil
}

func mergeAccessACL(
	entries []aclEntry,
	mode fs.FileMode,
	grants []aclEntry,
) ([]aclEntry, error) {
	if len(entries) == 0 {
		entries = []aclEntry{
			{
				Tag:         aclUserObject,
				Permissions: uint16(mode.Perm() >> 6),
				ID:          aclUndefinedID,
			},
			{
				Tag:         aclGroupObject,
				Permissions: uint16(mode.Perm()>>3) & 0o7,
				ID:          aclUndefinedID,
			},
			{
				Tag:         aclOther,
				Permissions: uint16(mode.Perm()) & 0o7,
				ID:          aclUndefinedID,
			},
		}
	} else {
		entries = append([]aclEntry(nil), entries...)
	}
	if err := validateAccessACL(entries); err != nil {
		return nil, err
	}

	maskPermissions, hasMask := aclPermissions(entries, aclMask, aclUndefinedID)
	if !hasMask {
		var ok bool
		maskPermissions, ok = aclPermissions(entries, aclGroupObject, aclUndefinedID)
		if !ok {
			return nil, fmt.Errorf("missing owning group entry")
		}
	}
	maskIndex := -1
	for index, entry := range entries {
		if entry.Permissions&^uint16(0o7) != 0 {
			return nil, fmt.Errorf("entry tag=%d has invalid permissions %#o", entry.Tag, entry.Permissions)
		}
		if entry.Tag == aclMask {
			maskIndex = index
		}
	}

	requiredMask := maskPermissions
	for _, grant := range grants {
		if grant.Tag != aclUser && grant.Tag != aclGroup {
			return nil, fmt.Errorf("unsupported named grant tag %d", grant.Tag)
		}
		if grant.Permissions&^uint16(0o7) != 0 {
			return nil, fmt.Errorf("grant tag=%d has invalid permissions %#o", grant.Tag, grant.Permissions)
		}
		requiredMask |= grant.Permissions
	}

	newlyEffective := requiredMask &^ maskPermissions
	if newlyEffective != 0 {
		for index := range entries {
			switch entries[index].Tag {
			case aclUser, aclGroupObject, aclGroup:
				entries[index].Permissions &^= newlyEffective
			}
		}
	}

	for _, grant := range grants {
		found := false
		for index := range entries {
			if entries[index].Tag == grant.Tag && entries[index].ID == grant.ID {
				entries[index].Permissions |= grant.Permissions
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, grant)
		}
	}

	if len(grants) > 0 {
		if maskIndex >= 0 {
			entries[maskIndex].Permissions = requiredMask
		} else {
			entries = append(entries, aclEntry{
				Tag:         aclMask,
				Permissions: requiredMask,
				ID:          aclUndefinedID,
			})
		}
	}

	if err := validateAccessACL(entries); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Tag == entries[j].Tag {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Tag < entries[j].Tag
	})
	return entries, nil
}

func validateAccessACL(entries []aclEntry) error {
	required := []uint16{aclUserObject, aclGroupObject, aclOther}
	for _, tag := range required {
		count := 0
		for _, entry := range entries {
			if entry.Tag == tag {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("ACL has %d entries for required tag %d", count, tag)
		}
	}

	seen := make(map[[2]uint32]struct{}, len(entries))
	hasNamed := false
	hasMask := false
	for _, entry := range entries {
		switch entry.Tag {
		case aclUserObject, aclGroupObject, aclMask, aclOther:
			if entry.ID != aclUndefinedID {
				return fmt.Errorf("ACL tag %d must use undefined ID", entry.Tag)
			}
		case aclUser, aclGroup:
			hasNamed = true
			if entry.ID == aclUndefinedID {
				return fmt.Errorf("named ACL tag %d has undefined ID", entry.Tag)
			}
		default:
			return fmt.Errorf("unsupported ACL tag %d", entry.Tag)
		}
		if entry.Tag == aclMask {
			if hasMask {
				return fmt.Errorf("ACL has duplicate mask entries")
			}
			hasMask = true
		}
		key := [2]uint32{uint32(entry.Tag), entry.ID}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("ACL has duplicate tag=%d id=%d", entry.Tag, entry.ID)
		}
		seen[key] = struct{}{}
	}
	if hasNamed && !hasMask {
		return fmt.Errorf("extended ACL is missing mask entry")
	}
	return nil
}

func decodeAccessACL(data []byte) ([]aclEntry, error) {
	if len(data) < 4 || (len(data)-4)%8 != 0 {
		return nil, fmt.Errorf("invalid access ACL length %d", len(data))
	}
	if version := binary.LittleEndian.Uint32(data[:4]); version != aclVersion {
		return nil, fmt.Errorf("unsupported access ACL version %#x", version)
	}
	entries := make([]aclEntry, 0, (len(data)-4)/8)
	for offset := 4; offset < len(data); offset += 8 {
		entries = append(entries, aclEntry{
			Tag:         binary.LittleEndian.Uint16(data[offset : offset+2]),
			Permissions: binary.LittleEndian.Uint16(data[offset+2 : offset+4]),
			ID:          binary.LittleEndian.Uint32(data[offset+4 : offset+8]),
		})
	}
	if err := validateAccessACL(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func encodeAccessACL(entries []aclEntry) []byte {
	data := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(data[:4], aclVersion)
	for index, entry := range entries {
		offset := 4 + 8*index
		binary.LittleEndian.PutUint16(data[offset:offset+2], entry.Tag)
		binary.LittleEndian.PutUint16(data[offset+2:offset+4], entry.Permissions)
		binary.LittleEndian.PutUint32(data[offset+4:offset+8], entry.ID)
	}
	return data
}

func aclPermissions(entries []aclEntry, tag uint16, id uint32) (uint16, bool) {
	for _, entry := range entries {
		if entry.Tag == tag && entry.ID == id {
			return entry.Permissions, true
		}
	}
	return 0, false
}

func aclContains(entries []aclEntry, want aclEntry) bool {
	permissions, ok := aclPermissions(entries, want.Tag, want.ID)
	if !ok {
		return false
	}
	if want.Tag == aclUser || want.Tag == aclGroup || want.Tag == aclGroupObject {
		mask, ok := aclPermissions(entries, aclMask, aclUndefinedID)
		if ok {
			permissions &= mask
		}
	}
	return permissions&want.Permissions == want.Permissions
}
