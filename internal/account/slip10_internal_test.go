package account

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"
)

// The ed25519 test vectors of SLIP-0010 itself. Vector 1 is quoted from the
// specification (its master key and chain code also appear in the README of
// the ed25519-hd-key npm package); both vectors were reproduced by two
// independent JavaScript implementations, ed25519-hd-key 1.3.0 and
// micro-ed25519-hdkey 0.1.2, which agreed on every node.
func TestSLIP10Ed25519SpecVectors(t *testing.T) {
	t.Parallel()

	const (
		seed1 = "000102030405060708090a0b0c0d0e0f"
		seed2 = "fffcf9f6f3f0edeae7e4e1dedbd8d5d2cfccc9c6c3c0bdbab7b4b1aeaba8a5a29f9c999693908d8a8784817e7b7875726f6c696663605d5a5754514e4b484542"
		h     = hardenedOffset
	)
	tests := []struct {
		name      string
		seed      string
		path      []uint32
		chainCode string
		private   string
		public    string
	}{
		{
			"1 m", seed1, nil,
			"90046a93de5380a72b5e45010748567d5ea02bbf6522f979e05c0d8d8ca9fffb",
			"2b4be7f19ee27bbf30c667b642d5f4aa69fd169872f8fc3059c08ebae2eb19e7",
			"00a4b2856bfec510abab89753fac1ac0e1112364e7d250545963f135f2a33188ed",
		},
		{
			"1 m/0H", seed1,
			[]uint32{h},
			"8b59aa11380b624e81507a27fedda59fea6d0b779a778918a2fd3590e16e9c69",
			"68e0fe46dfb67e368c75379acec591dad19df3cde26e63b93a8e704f1dade7a3",
			"008c8a13df77a28f3445213a0f432fde644acaa215fc72dcdf300d5efaa85d350c",
		},
		{
			"1 m/0H/1H", seed1,
			[]uint32{h, h + 1},
			"a320425f77d1b5c2505a6b1b27382b37368ee640e3557c315416801243552f14",
			"b1d0bad404bf35da785a64ca1ac54b2617211d2777696fbffaf208f746ae84f2",
			"001932a5270f335bed617d5b935c80aedb1a35bd9fc1e31acafd5372c30f5c1187",
		},
		{
			"1 m/0H/1H/2H", seed1,
			[]uint32{h, h + 1, h + 2},
			"2e69929e00b5ab250f49c3fb1c12f252de4fed2c1db88387094a0f8c4c9ccd6c",
			"92a5b23c0b8a99e37d07df3fb9966917f5d06e02ddbd909c7e184371463e9fc9",
			"00ae98736566d30ed0e9d2f4486a64bc95740d89c7db33f52121f8ea8f76ff0fc1",
		},
		{
			"1 m/0H/1H/2H/2H", seed1,
			[]uint32{h, h + 1, h + 2, h + 2},
			"8f6d87f93d750e0efccda017d662a1b31a266e4a6f5993b15f5c1f07f74dd5cc",
			"30d1dc7e5fc04c31219ab25a27ae00b50f6fd66622f6e9c913253d6511d1e662",
			"008abae2d66361c879b900d204ad2cc4984fa2aa344dd7ddc46007329ac76c429c",
		},
		{
			"1 m/0H/1H/2H/2H/1000000000H", seed1,
			[]uint32{h, h + 1, h + 2, h + 2, h + 1000000000},
			"68789923a0cac2cd5a29172a475fe9e0fb14cd6adb5ad98a3fa70333e7afa230",
			"8f94d394a8e8fd6b1bc2f3f49f5c47e385281d5c17e65324b0f62483e37e8793",
			"003c24da049451555d51a7014a37337aa4e12d41e485abccfa46b47dfb2af54b7a",
		},
		{
			"2 m", seed2, nil,
			"ef70a74db9c3a5af931b5fe73ed8e1a53464133654fd55e7a66f8570b8e33c3b",
			"171cb88b1b3c1db25add599712e36245d75bc65a1a5c9e18d76f9f2b1eab4012",
			"008fe9693f8fa62a4305a140b9764c5ee01e455963744fe18204b4fb948249308a",
		},
		{
			"2 m/0H", seed2,
			[]uint32{h},
			"0b78a3226f915c082bf118f83618a618ab6dec793752624cbeb622acb562862d",
			"1559eb2bbec5790b0c65d8693e4d0875b1747f4970ae8b650486ed7470845635",
			"0086fab68dcb57aa196c77c5f264f215a112c22a912c10d123b0d03c3c28ef1037",
		},
		{
			"2 m/0H/2147483647H", seed2,
			[]uint32{h, h + 2147483647},
			"138f0b2551bcafeca6ff2aa88ba8ed0ed8de070841f0c4ef0165df8181eaad7f",
			"ea4f5bfe8694d8bb74b7b59404632fd5968b774ed545e810de9c32a4fb4192f4",
			"005ba3b9ac6e90e83effcd25ac4e58a1365a9e35a3d3ae5eb07b9e4d90bcf7506d",
		},
		{
			"2 m/0H/2147483647H/1H", seed2,
			[]uint32{h, h + 2147483647, h + 1},
			"73bd9fff1cfbde33a1b846c27085f711c0fe2d66fd32e139d3ebc28e5a4a6b90",
			"3757c7577170179c7868353ada796c839135b3d30554bbb74a4b1e4a5a58505c",
			"002e66aa57069c86cc18249aecf5cb5a9cebbfd6fadeab056254763874a9352b45",
		},
		{
			"2 m/0H/2147483647H/1H/2147483646H", seed2,
			[]uint32{h, h + 2147483647, h + 1, h + 2147483646},
			"0902fe8a29f9140480a00ef244bd183e8a13288e4412d8389d140aac1794825a",
			"5837736c89570de861ebc173b1086da4f505d4adb387c6a1b1342d5e4ac9ec72",
			"00e33c0f7d81d843c572275f287498e8d408654fdf0d1e065b84e2e6f157aab09b",
		},
		{
			"2 m/0H/2147483647H/1H/2147483646H/2H", seed2,
			[]uint32{h, h + 2147483647, h + 1, h + 2147483646, h + 2},
			"5d70af781f3a37b829f0d060924d5e960bdc02e85423494afc0b1a41bbe196d4",
			"551d333177df541ad876a60ea71f00447931c0a9da16f227c11ea080d7391b8d",
			"0047150c75db263559a70d5778bf36abbab30fb061ad69f69ece61a72b0cfa4fc0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seed, err := hex.DecodeString(tt.seed)
			if err != nil {
				t.Fatal(err)
			}
			key, chainCode, err := slip10Ed25519(seed, tt.path...)
			if err != nil {
				t.Fatalf("slip10Ed25519() error = %v", err)
			}
			if got := hex.EncodeToString(chainCode); got != tt.chainCode {
				t.Errorf("chain code = %s, want %s", got, tt.chainCode)
			}
			if got := hex.EncodeToString(key); got != tt.private {
				t.Errorf("private key = %s, want %s", got, tt.private)
			}
			// SLIP-0010 writes the public key with a 0x00 prefix.
			public := ed25519.NewKeyFromSeed(key).Public().(ed25519.PublicKey)
			if got := "00" + hex.EncodeToString(public); got != tt.public {
				t.Errorf("public key = %s, want %s", got, tt.public)
			}
		})
	}
}

func TestSLIP10Ed25519RefusesNormalChildren(t *testing.T) {
	t.Parallel()

	for _, path := range [][]uint32{{0}, {hardenedOffset, hardenedOffset - 1}} {
		key, chainCode, err := slip10Ed25519([]byte("0123456789abcdef"), path...)
		if !errors.Is(err, errNotHardened) {
			t.Errorf("slip10Ed25519(%v) error = %v, want errNotHardened", path, err)
		}
		if key != nil || chainCode != nil {
			t.Errorf("slip10Ed25519(%v) returned key material with its error", path)
		}
	}
}

// Without the range check an index of 2^31 or more would wrap to a normal
// child; the refusal has to come from the check itself.
func TestDeriveEd25519RefusesIndexesWithoutAHardenedForm(t *testing.T) {
	t.Parallel()

	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	for _, index := range []uint32{hardenedOffset, hardenedOffset + 1, ^uint32(0)} {
		if _, err := DeriveEd25519PrivateKey(mnemonic, "", FamilySolana, index); !errors.Is(err, errIndexRange) {
			t.Errorf("DeriveEd25519PrivateKey(%d) error = %v, want errIndexRange", index, err)
		}
	}
}
