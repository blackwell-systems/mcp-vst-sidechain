// scan_test.go - a gated survey of a live plugin's parameters. It connects to a running Sidechain host, probes
// EVERY automatable param's value text (the same SampleText -> inferParam path the semantic layer uses), and
// prints what each param infers to, flagging any param that yields a clean analytic POWER fit (real = A*norm^P
// with the fit's worst error inside tolerance). This is the tool used to hunt for a real plugin that exercises
// the power-curve model end to end (see fitPower in infer.go). Gated on SIDECHAIN_SCAN_PORT/CATALOG so the
// normal suite skips it; not wired into CI (it is a survey aid, not an assertion).
//
//	SIDECHAIN_SCAN_PORT=52999 SIDECHAIN_SCAN_CATALOG=cat.json go test -run TestScanPowerFits -v .

package sidechain

import (
	"os"
	"strconv"
	"testing"
)

func TestScanPowerFits(t *testing.T) {
	port := os.Getenv("SIDECHAIN_SCAN_PORT")
	catPath := os.Getenv("SIDECHAIN_SCAN_CATALOG")
	if port == "" || catPath == "" {
		t.Skip("set SIDECHAIN_SCAN_PORT and SIDECHAIN_SCAN_CATALOG to run the live param survey")
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	cat, err := loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	p, _ := strconv.Atoi(port)
	lc, err := dialLive("127.0.0.1", p)
	if err != nil {
		t.Fatalf("dial live: %v", err)
	}
	defer lc.Close()

	var powerHits, numericHits int
	for _, pd := range cat.All() {
		samples, err := lc.SampleText(pd.ID, nil)
		if err != nil {
			t.Logf("probe %s (%s): error %v", pd.ID, pd.Label, err)
			continue
		}
		pi := inferParam(samples)
		if pi.Numeric {
			numericHits++
		}
		if pi.Fit != nil && pi.Fit.Model == "power" && pi.analyticReliable() {
			powerHits++
			t.Logf("POWER FIT: id=%s label=%q unit=%s range=%.4g..%.4g A=%.4g P=%.4g maxRelErr=%.4f%%",
				pd.ID, pd.Label, pi.Unit, pi.RealMin, pi.RealMax, pi.Fit.A, pi.Fit.B, pi.Fit.MaxRelErr*100)
		} else if pi.Numeric && pi.Unit != "" && pi.Fit != nil {
			// A numeric param with a real unit and a fitted model, but NOT a clean-within-tolerance power fit.
			// Logged so the survey shows the full landscape (which curves the plugin actually uses).
			t.Logf("      %s label=%q unit=%s curve=%s fit=%s(err=%.2f%%) range=%.4g..%.4g",
				pd.ID, pd.Label, pi.Unit, pi.Curve, pi.Fit.Model, pi.Fit.MaxRelErr*100, pi.RealMin, pi.RealMax)
		}
	}
	t.Logf("scanned %d params: %d numeric-with-text, %d clean POWER fits", len(cat.All()), numericHits, powerHits)
}
