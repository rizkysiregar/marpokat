//go:build integration

package app

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The purge of a deleted project removes its files from the real object storage, and only its files.
func TestRemovePrefixRemovesOneTenantsFilesOnly(t *testing.T) {
	st := testStorage(t)
	ctx := context.Background()
	gone, kept := uuid.NewString(), uuid.NewString()
	keys := map[string]string{}
	for _, tenant := range []string{gone, kept} {
		for i := 0; i < 3; i++ {
			k := tenant + "/" + uuid.NewString()
			if err := st.Put(ctx, k, strings.NewReader("x"), 1, "text/plain"); err != nil {
				t.Fatal(err)
			}
			keys[k] = tenant
		}
		// a thumbnail sits next to its file
		if err := st.Put(ctx, tenant+"/thumb.thumb.jpg", strings.NewReader("y"), 1, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.RemovePrefix(ctx, gone+"/")
	if err != nil || n != 4 {
		t.Fatalf("removed %d, %v", n, err)
	}
	for k, tenant := range keys {
		_, err := st.Size(ctx, k)
		if tenant == gone && err == nil {
			t.Errorf("%s is still there", k)
		}
		if tenant == kept && err != nil {
			t.Errorf("%s of the other tenant went: %v", k, err)
		}
	}
	if n, err := st.RemovePrefix(ctx, gone+"/"); err != nil || n != 0 {
		t.Fatalf("second time: %d %v", n, err)
	}
	for _, bad := range []string{"", gone} { // never the whole bucket, never a prefix that could match another id
		if _, err := st.RemovePrefix(ctx, bad); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
}
