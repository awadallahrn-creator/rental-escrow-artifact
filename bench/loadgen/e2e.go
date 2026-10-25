package main

// End-to-end functional scenarios on the live networks. The same five lifecycle scenarios used
// for gas measurement (S1-S5) are executed on both platforms with short real-time periods, and
// the resulting deposit allocations are compared with the specification. Negative identity
// checks exercise the platform-specific identity mechanisms.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Short real-time parameters shared by both platforms.
const (
	e2ePeriod = 4 // seconds
	e2eN      = 3 // periods
	e2eWindow = 8 // proposal / challenge / arbitration windows (seconds)
	unit      = 1_000_000
)

type scenario struct {
	Name         string
	PaidPeriods  int
	Claim        int64 // landlord damage claim (units of 1e6); 0 = none
	Path         string
	Award        int64
	WantLandlord int64 // expected deposit share to landlord (units)
	WantTenant   int64
}

var scenarios = []scenario{
	{"S1 punctual tenant, no damage", 3, 0, "zero-claim", 0, 0, 1000},
	{"S2 damage claim accepted", 3, 300, "accept", 0, 300, 700},
	{"S3 damage claim unchallenged", 3, 300, "finalize", 0, 300, 700},
	{"S4 one period unpaid, claim disputed, award reduced", 2, 300, "arbitrate", 100, 600, 400},
	{"S5 landlord inactive", 3, 0, "landlord-timeout", 0, 0, 1000},
	{"S6 arbitrator inactive (one period unpaid)", 2, 300, "arbitrator-timeout", 0, 500, 500},
}

type outcome struct {
	Platform, Scenario         string
	WantLandlord, WantTenant   int64
	GotLandlord, GotTenant     int64
	ReceivedLandlord, Received int64 // deposit tokens actually withdrawn
	Pass                       bool
	Note                       string
}

func writeOutcomes(out, platform string, rows []outcome) error {
	_ = os.MkdirAll(out, 0o755)
	f, err := os.Create(filepath.Join(out, platform+"_e2e.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"platform", "scenario", "want_landlord", "want_tenant", "got_landlord", "got_tenant", "withdrawn_landlord", "withdrawn_tenant", "pass", "note"})
	allPass := true
	for _, r := range rows {
		allPass = allPass && r.Pass
		_ = w.Write([]string{r.Platform, r.Scenario, fmt.Sprint(r.WantLandlord), fmt.Sprint(r.WantTenant), fmt.Sprint(r.GotLandlord),
			fmt.Sprint(r.GotTenant), fmt.Sprint(r.ReceivedLandlord), fmt.Sprint(r.Received), strconv.FormatBool(r.Pass), r.Note})
		fmt.Printf("%-8s %-55s want L=%4d T=%4d  got L=%4d T=%4d  withdrawn L=%4d T=%4d  %v %s\n", r.Platform, r.Scenario,
			r.WantLandlord, r.WantTenant, r.GotLandlord, r.GotTenant, r.ReceivedLandlord, r.Received, r.Pass, r.Note)
	}
	w.Flush()
	if !allPass {
		return fmt.Errorf("%s: not all e2e checks passed", platform)
	}
	return nil
}

func sleepUntil(t time.Time) {
	if d := time.Until(t); d > 0 {
		time.Sleep(d)
	}
}

// ===================================================================== Fabric
func fabricE2E(ctx context.Context, fb *FabricBackend, cryptoDir, out string) error {
	cas := map[int]*fabCA{}
	for org := 1; org <= 3; org++ {
		ca, err := loadCA(cryptoDir, org)
		if err != nil {
			return err
		}
		cas[org] = ca
	}
	exp := time.Now().Add(24 * time.Hour)
	fb.st = &fabState{}
	minter, err := fb.minter(ctx, cas[1])
	if err != nil {
		return err
	}
	arb, _ := cas[3].issue("e2e-arbitrator", nil, exp)
	if err := fb.resolveAccount(&arb); err != nil {
		return err
	}
	tag := strconv.FormatInt(time.Now().Unix(), 36)
	bal := func(acct string) int64 {
		b, err := fb.evaluate(minter, "token", "BalanceOf", acct)
		if err != nil {
			return -1
		}
		v, _ := strconv.ParseInt(string(b), 10, 64)
		return v
	}
	var rows []outcome
	for si, sc := range scenarios {
		prop := fmt.Sprintf("did:example:property:e2e-%s-%d", tag, si)
		l, _ := cas[1].issue(fmt.Sprintf("e2e-l-%s-%d", tag, si), map[string]string{"rental.role": "landlord", "rental.did": "did:example:l" + tag, "rental.property": prop}, exp)
		t, _ := cas[2].issue(fmt.Sprintf("e2e-t-%s-%d", tag, si), map[string]string{"rental.role": "tenant", "rental.did": "did:example:t" + tag}, exp)
		if err := fb.resolveAccount(&l); err != nil {
			return err
		}
		if err := fb.resolveAccount(&t); err != nil {
			return err
		}
		id := fmt.Sprintf("e2e-%s-%d", tag, si)
		steps := [][]any{
			{minter, "token", "Mint", t.Account, strconv.Itoa(100_000 * unit)},
			{l, "rental", "CreateLease", id, fabTerms(l, t, arb, prop, e2ePeriod, e2eN, e2eWindow, e2eWindow, e2eWindow)},
			{l, "rental", "BindIdentity", id},
			{t, "rental", "BindIdentity", id},
			{t, "rental", "LockDeposit", id},
		}
		for _, s := range steps {
			args := make([]string, 0)
			for _, a := range s[3:] {
				args = append(args, a.(string))
			}
			if err := fb.exec(ctx, s[0].(fabIdentity), s[1].(string), s[2].(string), args...); err != nil {
				return fmt.Errorf("%s: %w", sc.Name, err)
			}
		}
		lease := func() map[string]any {
			b, _ := fb.evaluate(minter, "rental", "GetLease", id)
			m := map[string]any{}
			_ = json.Unmarshal(b, &m)
			return m
		}
		num := func(m map[string]any, k string) int64 { v, _ := m[k].(float64); return int64(v) }
		lm := lease()
		start, end := num(lm, "leaseStart"), num(lm, "leaseEnd")
		for p := 0; p < sc.PaidPeriods; p++ {
			sleepUntil(time.Unix(start+int64(p)*e2ePeriod, 0).Add(200 * time.Millisecond))
			if err := fb.exec(ctx, t, "rental", "PayRent", id); err != nil {
				return fmt.Errorf("%s pay %d: %w", sc.Name, p, err)
			}
		}
		lBefore, tBefore := bal(l.Account), bal(t.Account)
		rentReceived := lBefore // landlord received only rent so far
		sleepUntil(time.Unix(end, 0).Add(500 * time.Millisecond))
		claim := strconv.FormatInt(sc.Claim*unit, 10)
		var err error
		switch sc.Path {
		case "zero-claim":
			err = fb.exec(ctx, l, "rental", "ProposeSettlement", id, "0", "")
		case "accept":
			if err = fb.exec(ctx, l, "rental", "ProposeSettlement", id, claim, "ipfs://report"); err == nil {
				err = fb.exec(ctx, t, "rental", "AcceptSettlement", id)
			}
		case "finalize":
			if err = fb.exec(ctx, l, "rental", "ProposeSettlement", id, claim, "ipfs://report"); err == nil {
				sleepUntil(time.Now().Add((e2eWindow + 2) * time.Second))
				err = fb.exec(ctx, arb, "rental", "FinalizeSettlement", id)
			}
		case "arbitrate":
			if err = fb.exec(ctx, l, "rental", "ProposeSettlement", id, claim, "ipfs://report"); err == nil {
				if err = fb.exec(ctx, t, "rental", "DisputeSettlement", id, "ipfs://tenant"); err == nil {
					err = fb.exec(ctx, arb, "rental", "ResolveDispute", id, strconv.FormatInt(sc.Award*unit, 10))
				}
			}
		case "landlord-timeout":
			sleepUntil(time.Unix(end+e2eWindow+2, 0))
			err = fb.exec(ctx, t, "rental", "ClaimAfterTimeout", id)
		case "arbitrator-timeout":
			if err = fb.exec(ctx, l, "rental", "ProposeSettlement", id, claim, "ipfs://report"); err == nil {
				if err = fb.exec(ctx, t, "rental", "DisputeSettlement", id, "ipfs://tenant"); err == nil {
					sleepUntil(time.Now().Add((e2eWindow + 2) * time.Second))
					err = fb.exec(ctx, arb, "rental", "ClaimAfterTimeout", id)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", sc.Name, err)
		}
		lm = lease()
		if num(lm, "withdrawableLandlord") > 0 {
			if err := fb.exec(ctx, l, "rental", "Withdraw", id); err != nil {
				return err
			}
		}
		if num(lm, "withdrawableTenant") > 0 {
			if err := fb.exec(ctx, t, "rental", "Withdraw", id); err != nil {
				return err
			}
		}
		gotL, gotT := num(lm, "paidToLandlord")/unit, num(lm, "refundedToTenant")/unit
		wL, wT := (bal(l.Account)-rentReceived)/unit, (bal(t.Account)-tBefore)/unit
		rows = append(rows, outcome{Platform: "fabric", Scenario: sc.Name, WantLandlord: sc.WantLandlord, WantTenant: sc.WantTenant,
			GotLandlord: gotL, GotTenant: gotT, ReceivedLandlord: wL, Received: wT,
			Pass: gotL == sc.WantLandlord && gotT == sc.WantTenant && wL == sc.WantLandlord && wT == sc.WantTenant})
	}

	// ---- negative identity checks enforced by the platform (MSP) and by the chaincode
	prop := "did:example:property:neg-" + tag
	l, _ := cas[1].issue("neg-l-"+tag, map[string]string{"rental.role": "landlord", "rental.did": "did:example:nl", "rental.property": prop}, exp)
	t, _ := cas[2].issue("neg-t-"+tag, map[string]string{"rental.role": "tenant", "rental.did": "did:example:nt"}, exp)
	_ = fb.resolveAccount(&l)
	_ = fb.resolveAccount(&t)
	id := "neg-" + tag
	if err := fb.exec(ctx, l, "rental", "CreateLease", id, fabTerms(l, t, arb, prop, e2ePeriod, e2eN, e2eWindow, e2eWindow, e2eWindow)); err != nil {
		return err
	}
	// (a) expired certificate issued by a trusted CA: rejected by the peer's MSP before execution
	expired, _ := cas[2].issue("neg-expired-"+tag, map[string]string{"rental.role": "tenant", "rental.did": "did:example:nt"}, time.Now().Add(-time.Hour))
	err = fb.exec(ctx, expired, "rental", "BindIdentity", id)
	rows = append(rows, outcome{Platform: "fabric", Scenario: "N1 expired certificate rejected", Pass: err != nil, Note: noteOf(err)})
	// (b) certificate from a CA that is not a channel member (untrusted issuer)
	rogue, _ := selfSignedCA()
	rid, _ := rogue.issue("neg-rogue-"+tag, map[string]string{"rental.role": "tenant", "rental.did": "did:example:nt"}, exp)
	rid.MSP = "Org2MSP"
	err = fb.exec(ctx, rid, "rental", "BindIdentity", id)
	rows = append(rows, outcome{Platform: "fabric", Scenario: "N2 certificate from untrusted CA rejected", Pass: err != nil, Note: noteOf(err)})
	// (c) trusted certificate without the required attributes
	plain, _ := cas[2].issue("neg-plain-"+tag, nil, exp)
	_ = fb.resolveAccount(&plain)
	id2 := "neg2-" + tag
	_ = fb.exec(ctx, l, "rental", "CreateLease", id2, fabTerms(l, plain, arb, prop, e2ePeriod, e2eN, e2eWindow, e2eWindow, e2eWindow))
	err = fb.exec(ctx, plain, "rental", "BindIdentity", id2)
	rows = append(rows, outcome{Platform: "fabric", Scenario: "N3 certificate without role/DID attributes rejected", Pass: err != nil && strings.Contains(err.Error(), "IdentityMismatch"), Note: noteOf(err)})
	// (d) a third party cannot pay, propose or resolve
	err = fb.exec(ctx, l, "rental", "PayRent", id)
	rows = append(rows, outcome{Platform: "fabric", Scenario: "N4 non-tenant rent payment rejected", Pass: err != nil && strings.Contains(err.Error(), "Unauthorized"), Note: noteOf(err)})
	return writeOutcomes(out, "fabric", rows)
}

func noteOf(err error) string {
	if err == nil {
		return "UNEXPECTED SUCCESS"
	}
	s := err.Error()
	for _, k := range []string{"expired", "IdentityMismatch", "Unauthorized", "access denied", "creator", "certificate", "revert", "UntrustedIssuer"} {
		if strings.Contains(s, k) {
			return "rejected (" + k + ")"
		}
	}
	return "rejected: " + shorten(s)
}

// selfSignedCA creates a fresh CA that is not part of any channel MSP, with the same subject as
// Org2's CA, to show that subject names alone do not make an issuer trusted.
func selfSignedCA() (*fabCA, error) {
	ref, err := loadCA("/opt/bench/fabnet/crypto", 2)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: ref.cert.Subject,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		SubjectKeyId: []byte{1, 2, 3, 4},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &fabCA{cert: cert, key: key, msp: "Org2MSP"}, nil
}

// ===================================================================== Besu
func besuE2E(ctx context.Context, bb *BesuBackend, out string) error {
	seed := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	n := len(scenarios) + 2
	bb.accounts(seed, n)
	for _, a := range append(append([]*account{bb.deployer, bb.issuer, bb.arbitrator}, bb.landlords...), bb.tenants...) {
		a.nonce, _ = bb.ec.PendingNonceAt(ctx, a.addr)
	}
	registry, err := bb.deploy(ctx, bb.deployer, "IdentityRegistry", bb.deployer.addr)
	if err != nil {
		return err
	}
	if _, err := bb.exec(ctx, bb.deployer, &registry, bb.call("IdentityRegistry", "addIssuer", bb.issuer.addr), 200_000, "addIssuer"); err != nil {
		return err
	}
	token, err := bb.deploy(ctx, bb.deployer, "PlainStablecoin")
	if err != nil {
		return err
	}
	callU := func(to common.Address, art, m string, args ...any) *big.Int {
		o, err := bb.ec.CallContract(ctx, ethCall(to, bb.call(art, m, args...)), nil)
		if err != nil {
			return big.NewInt(-1)
		}
		r, _ := bb.art[art].ABI.Unpack(m, o)
		switch v := r[0].(type) {
		case *big.Int:
			return v
		case uint64:
			return new(big.Int).SetUint64(v)
		}
		return big.NewInt(-1)
	}
	bal := func(a common.Address) int64 { return callU(token, "PlainStablecoin", "balanceOf", a).Int64() }
	blockTime := func() int64 {
		h, err := bb.ec.HeaderByNumber(ctx, nil)
		if err != nil {
			return 0
		}
		return int64(h.Time)
	}
	waitBlockTime := func(t int64) {
		for blockTime() <= t {
			time.Sleep(500 * time.Millisecond)
		}
	}
	newLease := func(i int) (common.Address, error) {
		prop := crypto.Keccak256Hash([]byte(fmt.Sprintf("did:example:property:%s-%d", seed, i)))
		lease, err := bb.deploy(ctx, bb.deployer, "RentalAgreement", benchTerms(bb.landlords[i].addr, bb.tenants[i].addr,
			bb.arbitrator.addr, token, prop, e2ePeriod, e2eN, e2eWindow, e2eWindow, e2eWindow), registry)
		if err != nil {
			return lease, err
		}
		if _, err := bb.exec(ctx, bb.deployer, &token, bb.call("PlainStablecoin", "mint", bb.tenants[i].addr, big.NewInt(100_000*unit)), 200_000, "mint"); err != nil {
			return lease, err
		}
		return lease, nil
	}
	activate := func(i int, lease common.Address) error {
		prop := crypto.Keccak256Hash([]byte(fmt.Sprintf("did:example:property:%s-%d", seed, i)))
		la, ls := bb.attest(registry, bb.landlords[i].addr, "did:example:l"+seed, 1, prop)
		ta, ts := bb.attest(registry, bb.tenants[i].addr, "did:example:t"+seed, 2, [32]byte{})
		maxU := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
		for _, s := range []struct {
			from *account
			to   common.Address
			data []byte
			what string
		}{
			{bb.landlords[i], lease, bb.call("RentalAgreement", "bindIdentity", la, ls), "bind landlord"},
			{bb.tenants[i], lease, bb.call("RentalAgreement", "bindIdentity", ta, ts), "bind tenant"},
			{bb.tenants[i], token, bb.call("PlainStablecoin", "approve", lease, maxU), "approve"},
			{bb.tenants[i], lease, bb.call("RentalAgreement", "lockDeposit"), "lockDeposit"},
		} {
			to := s.to
			if _, err := bb.exec(ctx, s.from, &to, s.data, 400_000, s.what); err != nil {
				return err
			}
		}
		return nil
	}
	var rows []outcome
	for i, sc := range scenarios {
		lease, err := newLease(i)
		if err != nil {
			return fmt.Errorf("%s: %w", sc.Name, err)
		}
		if err := activate(i, lease); err != nil {
			return fmt.Errorf("%s: %w", sc.Name, err)
		}
		start := callU(lease, "RentalAgreement", "leaseStart").Int64()
		end := callU(lease, "RentalAgreement", "leaseEnd").Int64()
		L, T := bb.landlords[i], bb.tenants[i]
		do := func(from *account, what string, m string, args ...any) error {
			_, err := bb.exec(ctx, from, &lease, bb.call("RentalAgreement", m, args...), 400_000, what)
			return err
		}
		for p := 0; p < sc.PaidPeriods; p++ {
			waitBlockTime(start + int64(p)*e2ePeriod - e2ePeriod) // eligible one period ahead
			if err := do(T, "payRent", "payRent"); err != nil {
				return fmt.Errorf("%s pay %d: %w", sc.Name, p, err)
			}
		}
		rentReceived, tBefore := bal(L.addr), bal(T.addr)
		waitBlockTime(end)
		claim := big.NewInt(sc.Claim * unit)
		report, tenantEv := crypto.Keccak256Hash([]byte("ipfs://report")), crypto.Keccak256Hash([]byte("ipfs://tenant"))
		switch sc.Path {
		case "zero-claim":
			err = do(L, "propose", "proposeSettlement", big.NewInt(0), [32]byte{})
		case "accept":
			if err = do(L, "propose", "proposeSettlement", claim, report); err == nil {
				err = do(T, "accept", "acceptSettlement")
			}
		case "finalize":
			if err = do(L, "propose", "proposeSettlement", claim, report); err == nil {
				waitBlockTime(callU(lease, "RentalAgreement", "challengeDeadline").Int64())
				err = do(bb.arbitrator, "finalize", "finalizeSettlement")
			}
		case "arbitrate":
			if err = do(L, "propose", "proposeSettlement", claim, report); err == nil {
				if err = do(T, "dispute", "disputeSettlement", tenantEv); err == nil {
					err = do(bb.arbitrator, "resolve", "resolveDispute", big.NewInt(sc.Award*unit))
				}
			}
		case "landlord-timeout":
			waitBlockTime(end + e2eWindow)
			err = do(T, "timeout", "claimAfterTimeout")
		case "arbitrator-timeout":
			if err = do(L, "propose", "proposeSettlement", claim, report); err == nil {
				if err = do(T, "dispute", "disputeSettlement", tenantEv); err == nil {
					waitBlockTime(callU(lease, "RentalAgreement", "arbitrationDeadline").Int64())
					err = do(bb.arbitrator, "timeout", "claimAfterTimeout")
				}
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", sc.Name, err)
		}
		gotL := callU(lease, "RentalAgreement", "paidToLandlord").Int64() / unit
		gotT := callU(lease, "RentalAgreement", "refundedToTenant").Int64() / unit
		if gotL > 0 {
			if err := do(L, "withdraw", "withdraw"); err != nil {
				return err
			}
		}
		if gotT > 0 {
			if err := do(T, "withdraw", "withdraw"); err != nil {
				return err
			}
		}
		wL, wT := (bal(L.addr)-rentReceived)/unit, (bal(T.addr)-tBefore)/unit
		rows = append(rows, outcome{Platform: "besu", Scenario: sc.Name, WantLandlord: sc.WantLandlord, WantTenant: sc.WantTenant,
			GotLandlord: gotL, GotTenant: gotT, ReceivedLandlord: wL, Received: wT,
			Pass: gotL == sc.WantLandlord && gotT == sc.WantTenant && wL == sc.WantLandlord && wT == sc.WantTenant})
	}

	// ---- negative identity checks enforced by the registry contract
	i := len(scenarios)
	lease, err := newLease(i)
	if err != nil {
		return err
	}
	prop := crypto.Keccak256Hash([]byte(fmt.Sprintf("did:example:property:%s-%d", seed, i)))
	T := bb.tenants[i]
	try := func(from *account, data []byte) error {
		_, err := bb.exec(ctx, from, &lease, data, 400_000, "neg")
		return err
	}
	// (a) expired attestation
	a, _ := bb.attest(registry, T.addr, "did:example:t", 2, [32]byte{})
	a.ExpiresAt = uint64(time.Now().Add(-time.Hour).Unix())
	sig, _ := crypto.Sign(attestationDigest(registry, a).Bytes(), bb.issuer.key)
	sig[64] += 27
	err = try(T, bb.call("RentalAgreement", "bindIdentity", a, sig))
	rows = append(rows, outcome{Platform: "besu", Scenario: "N1 expired attestation rejected", Pass: err != nil, Note: noteOf(err)})
	// (b) attestation signed by an untrusted issuer
	rogue := deriveKey(seed, "rogue")
	a, _ = bb.attest(registry, T.addr, "did:example:t", 2, [32]byte{})
	sig, _ = crypto.Sign(attestationDigest(registry, a).Bytes(), rogue)
	sig[64] += 27
	err = try(T, bb.call("RentalAgreement", "bindIdentity", a, sig))
	rows = append(rows, outcome{Platform: "besu", Scenario: "N2 attestation from untrusted issuer rejected", Pass: err != nil, Note: noteOf(err)})
	// (c) revoked attestation
	a, sig = bb.attest(registry, T.addr, "did:example:t-revoked", 2, [32]byte{})
	if _, err := bb.exec(ctx, bb.issuer, &registry, bb.call("IdentityRegistry", "revoke", a, sig), 200_000, "revoke"); err != nil {
		return err
	}
	err = try(T, bb.call("RentalAgreement", "bindIdentity", a, sig))
	rows = append(rows, outcome{Platform: "besu", Scenario: "N3 revoked attestation rejected", Pass: err != nil, Note: noteOf(err)})
	// (d) third party cannot pay rent
	err = try(bb.landlords[i], bb.call("RentalAgreement", "payRent"))
	rows = append(rows, outcome{Platform: "besu", Scenario: "N4 non-tenant rent payment rejected", Pass: err != nil, Note: noteOf(err)})
	_ = prop
	return writeOutcomes(out, "besu", rows)
}
