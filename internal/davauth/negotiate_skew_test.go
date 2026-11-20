package davauth

import (
	"testing"
	"time"

	kerb "github.com/hstern/go-oauth2-kerberos-exchange"
	"github.com/hstern/krb5/credentials"
	"github.com/hstern/krb5/iana"
	"github.com/hstern/krb5/iana/nametype"
	"github.com/hstern/krb5/messages"
	"github.com/hstern/krb5/types"
)

// holderOfKeyCredential builds a kerbexchange-style credential: a ccache with a
// single service ticket (no TGT) whose StartTime is start relative to now, in
// whole seconds as kerbexchanged stores it.
func holderOfKeyCredential(t *testing.T, spn kerb.ServicePrincipal, start, end time.Duration) *kerb.Credential {
	t.Helper()

	sname := types.PrincipalName{NameType: nametype.KRB_NT_SRV_HST, NameString: []string{spn.Service, spn.Host}}
	tkt := messages.Ticket{
		TktVNO:  iana.PVNO,
		Realm:   spn.Realm,
		SName:   sname,
		EncPart: types.EncryptedData{EType: 18, KVNO: 1, Cipher: []byte("ciphertext")},
	}
	tb, err := tkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	client := credentials.NewPrincipal(types.NewPrincipalName(nametype.KRB_NT_PRINCIPAL, "alice"), spn.Realm)
	cc := credentials.NewV4CCache()
	cc.SetDefaultPrincipal(client)
	cc.AddCredential(&credentials.Credential{
		Client:      client,
		Server:      credentials.NewPrincipal(sname, spn.Realm),
		Key:         types.EncryptionKey{KeyType: 18, KeyValue: []byte("0123456789abcdef0123456789abcdef")},
		AuthTime:    now.Add(start),
		StartTime:   now.Add(start),
		EndTime:     now.Add(end),
		RenewTill:   time.Unix(0, 0),
		TicketFlags: types.NewKrbFlags(),
		Ticket:      tb,
	})
	raw, err := cc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return kerb.NewCredential("alice", spn, now.Add(end), raw, nil)
}

// MB720-61: kerbexchanged stamps StartTime from its own clock. If the gateway's
// clock is behind, the ticket's StartTime is slightly in the gateway's future and
// the cached-ticket lookup must still serve it (within libdefaults clockskew),
// since a holder-of-key ccache has no TGT to fall back to a TGS exchange.
func TestKerberosTokenSourceStartTimeWithinClockSkew(t *testing.T) {
	spn := kerb.ServicePrincipal{Service: "HTTP", Host: "dav.example.com", Realm: "EXAMPLE.COM"}

	t.Run("start 5s in the future is served", func(t *testing.T) {
		src, err := KerberosTokenSource(holderOfKeyCredential(t, spn, 5*time.Second, time.Hour), spn)
		if err != nil {
			t.Fatalf("KerberosTokenSource: %v", err)
		}
		tok, err := src()
		if err != nil {
			t.Fatalf("mint token with StartTime 5s ahead: %v", err)
		}
		if tok == "" {
			t.Fatal("empty token")
		}
	})

	t.Run("start far beyond the skew is not served", func(t *testing.T) {
		src, err := KerberosTokenSource(holderOfKeyCredential(t, spn, time.Hour, 2*time.Hour), spn)
		if err != nil {
			t.Fatalf("KerberosTokenSource: %v", err)
		}
		if _, err := src(); err == nil {
			t.Fatal("expected error for a ticket not yet valid beyond clock skew")
		}
	})
}
