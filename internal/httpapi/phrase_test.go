package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
)

// An older client still asking for an imported-only space gets an ordinary
// one, phrase and all, rather than an error or a space nothing can restore.
func TestPlatformGivesEveryNewSpaceAPhrase(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces", map[string]any{
		"password": "a long enough password", "imported_only": true,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /api/spaces = %d %s", created.Code, created.Body.String())
	}
	body := decodeBody[struct {
		Space struct {
			ID        string `json:"id"`
			HasPhrase bool   `json:"has_phrase"`
		} `json:"space"`
		MnemonicGenerated bool   `json:"mnemonic_generated"`
		Mnemonic          string `json:"mnemonic"`
	}](t, created)
	if !body.MnemonicGenerated || body.Mnemonic == "" || !body.Space.HasPhrase {
		t.Fatalf("create = %+v, want a generated phrase", body)
	}

	path := "/api/spaces/" + body.Space.ID + "/recovery-phrase"
	wrong := platformRequest(t, fixture.handler, http.MethodPost, path, map[string]string{"password": "not the password"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("recovery-phrase with a wrong password = %d %s", wrong.Code, wrong.Body.String())
	}
	again := platformRequest(t, fixture.handler, http.MethodPost, path, map[string]string{"password": "a long enough password"})
	if again.Code != http.StatusConflict {
		t.Fatalf("recovery-phrase on a space that has one = %d %s", again.Code, again.Body.String())
	}
}

// The list of spaces cannot say whether a locked space has a phrase. The
// unlock answer can, so the browser does not offer to create a phrase for a
// space that has one in the moment between unlocking and the next poll.
func TestPlatformUnlockAnswersWithTheOpenSpace(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	id := createSpace(t, fixture, "Main", "a long enough password")
	if locked := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/lock", map[string]any{}); locked.Code != http.StatusOK {
		t.Fatalf("lock = %d", locked.Code)
	}
	listed := decodeBody[struct {
		Spaces []struct {
			HasPhrase bool `json:"has_phrase"`
		} `json:"spaces"`
	}](t, platformRequest(t, fixture.handler, http.MethodGet, "/api/spaces", nil))
	if listed.Spaces[0].HasPhrase {
		t.Fatal("a locked space claims to know about its phrase")
	}
	unlocked := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/unlock",
		map[string]string{"password": "a long enough password"})
	if unlocked.Code != http.StatusOK {
		t.Fatalf("unlock = %d %s", unlocked.Code, unlocked.Body.String())
	}
	body := decodeBody[struct {
		Unlocked bool `json:"unlocked"`
		Space    struct {
			ID        string `json:"id"`
			Locked    bool   `json:"locked"`
			HasPhrase bool   `json:"has_phrase"`
		} `json:"space"`
	}](t, unlocked)
	if !body.Unlocked || body.Space.ID != id || body.Space.Locked || !body.Space.HasPhrase {
		t.Fatalf("unlock answer = %+v", body)
	}
}

func TestPlatformTakesAPhraseFromAnotherComputer(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	id := createSpace(t, fixture, "Main", "a long enough password")
	// The space has a phrase already; the point here is that a given phrase
	// is checked before anything else.
	bad := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/recovery-phrase",
		map[string]string{"password": "a long enough password", "mnemonic": "not a real phrase"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("recovery-phrase with an invalid phrase = %d %s", bad.Code, bad.Body.String())
	}
}

// spaceWithoutPhrase is a bundle holding one space the way versions before
// every space had a phrase made an imported-only one: a vault with no
// mnemonic. It is put together on a side home and exported from there.
func spaceWithoutPhrase(t *testing.T, id, password string) []byte {
	t.Helper()
	home := t.TempDir()
	params := vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}
	now := time.Now().UTC()
	container, err := vault.SealJSON(password, space.Payload{
		Version: 1, ImportedKeys: map[string]space.KeyVariants{}, CreatedAt: now,
	}, fmt.Appendf(nil, "%s:%d", id, space.SchemaVersion), params)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(space.File{
		SchemaVersion: space.SchemaVersion, ID: id, Name: "imported only",
		CreatedAt: now, UpdatedAt: now, Vault: container,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "spaces", id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "spaces", id, "space.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	side, err := space.NewManager(home, 15*time.Minute, params)
	if err != nil {
		t.Fatal(err)
	}
	defer side.Close()
	bundle, err := side.ExportBundle([]space.ExportItem{{ID: id, Password: password}})
	if err != nil {
		t.Fatalf("ExportBundle() error = %v", err)
	}
	return bundle
}

// A phrase that is already another space's here would merge the two for
// good. It is a conflict, and the space stays without a phrase.
func TestPlatformRefusesAnotherLocalSpacesPhrase(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces", map[string]any{
		"password": "a long enough password",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /api/spaces = %d %s", created.Code, created.Body.String())
	}
	other := decodeBody[struct {
		Mnemonic string `json:"mnemonic"`
	}](t, created)

	const id, password = "spc_phraseless", "an older space password"
	if _, err := fixture.spaces.ImportBundle(spaceWithoutPhrase(t, id, password)); err != nil {
		t.Fatalf("ImportBundle() error = %v", err)
	}
	if unlocked := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/unlock",
		map[string]string{"password": password}); unlocked.Code != http.StatusOK {
		t.Fatalf("unlock = %d %s", unlocked.Code, unlocked.Body.String())
	}

	refused := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/recovery-phrase",
		map[string]string{"password": password, "mnemonic": other.Mnemonic})
	if refused.Code != http.StatusConflict {
		t.Fatalf("recovery-phrase with another space's phrase = %d %s", refused.Code, refused.Body.String())
	}
	got := decodeBody[struct {
		Space struct {
			HasPhrase bool `json:"has_phrase"`
		} `json:"space"`
	}](t, platformRequest(t, fixture.handler, http.MethodGet, "/api/spaces/"+id, nil))
	if got.Space.HasPhrase {
		t.Fatal("the refused phrase was kept")
	}
}
