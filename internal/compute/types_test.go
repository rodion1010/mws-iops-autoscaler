package compute

import "testing"

func TestStorageDisk_IdentityKey_differsAcrossIncarnations(t *testing.T) {
	original := StorageDisk{DiskName: "vm-1-data", IncarnationID: "incarnation-1"}
	recreated := StorageDisk{DiskName: "vm-1-data", IncarnationID: "incarnation-2"}

	if original.IdentityKey() == recreated.IdentityKey() {
		t.Errorf("IdentityKey() collided (%q) for two different incarnations of the same DiskName", original.IdentityKey())
	}
}

func TestStorageDisk_IdentityKey_differsAcrossDiskNames(t *testing.T) {
	a := StorageDisk{DiskName: "vm-1-data", IncarnationID: "same-incarnation"}
	b := StorageDisk{DiskName: "vm-1-boot", IncarnationID: "same-incarnation"}

	if a.IdentityKey() == b.IdentityKey() {
		t.Errorf("IdentityKey() collided (%q) for two disks with different DiskName", a.IdentityKey())
	}
}

func TestStorageDisk_IdentityKey_stableForTheSameDisk(t *testing.T) {
	a := StorageDisk{DiskName: "vm-1-data", IncarnationID: "incarnation-1"}
	b := StorageDisk{DiskName: "vm-1-data", IncarnationID: "incarnation-1"}

	if a.IdentityKey() != b.IdentityKey() {
		t.Errorf("IdentityKey() differed (%q vs %q) for the identical DiskName/IncarnationID pair", a.IdentityKey(), b.IdentityKey())
	}
}
