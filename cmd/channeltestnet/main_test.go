package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchUTXOsAndPick(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mxCz/unspent", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"height":100,"tx_pos":0,"tx_hash":"aa","value":1000},
			{"height":101,"tx_pos":2,"tx_hash":"bb","value":50000},
			{"height":102,"tx_pos":1,"tx_hash":"cc","value":7000}
		]`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	us, err := fetchUTXOs(ts.URL, "mxCz")
	if err != nil {
		t.Fatal(err)
	}
	if len(us) != 3 || us[1].TxID != "bb" || us[1].Vout != 2 || us[1].Value != 50000 {
		t.Fatalf("parsed utxos wrong: %+v", us)
	}
	// pick the largest covering the need.
	best, err := pickUTXO(ts.URL, "mxCz", 8000)
	if err != nil || best.TxID != "bb" {
		t.Fatalf("pick = %+v err=%v (want bb)", best, err)
	}
	// nothing big enough -> error.
	if _, err := pickUTXO(ts.URL, "mxCz", 60000); err == nil {
		t.Fatal("expected error when no UTXO covers the need")
	}
}

func TestFetchStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/deadbeef", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"confirmations":6,"blockheight":1600123}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	conf, height, err := fetchStatus(ts.URL, "deadbeef")
	if err != nil || conf != 6 || height != 1600123 {
		t.Fatalf("status conf=%d height=%d err=%v", conf, height, err)
	}
}

func TestGetJSONHandlesError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer ts.Close()
	var v any
	if err := getJSON(ts.URL+"/x", &v); err == nil {
		t.Fatal("expected error on 502")
	}
}
