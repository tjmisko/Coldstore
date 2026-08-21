package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tjmisko/coldstore/internal/catalog"
)

func TestHandleUpdateFileDescriptionAndManualMetadata(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	_, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.jpg", Filename: "a.jpg",
		Extension: ".jpg", Status: catalog.StatusSynced, FileMtime: time.Now(),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("TestFolder", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"description":"jellyfish","captured_local":"2024-02-29 03:04:05.006","gps_lat":0,"gps_lon":0}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/files/"+itoa(file.ID), bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got catalog.FileRecord
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Description != "jellyfish" || got.CapturedSource != "manual" || got.GPSSource != "manual" ||
		got.GPSLat == nil || got.GPSLon == nil || *got.GPSLat != 0 || *got.GPSLon != 0 {
		t.Fatalf("response = %+v", got)
	}
}

func TestHandleUpdateFileValidation(t *testing.T) {
	srv, cat, _ := setupTestServer(t)
	_, err := cat.UpsertFile(&catalog.FileRecord{
		FolderLabel: "TestFolder", RelativePath: "a.jpg", Filename: "a.jpg",
		Extension: ".jpg", Status: catalog.StatusSynced, FileMtime: time.Now(),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	file, err := cat.GetFile("TestFolder", "a.jpg")
	if err != nil {
		t.Fatal(err)
	}

	tests := []string{
		`{"description":"` + strings.Repeat("界", maxDescriptionCharacters+1) + `"}`,
		`{"captured_local":"2024-02-30 03:04:05"}`,
		`{"captured_local":"2024-02-29T03:04:05Z"}`,
		`{"gps_lat":12}`,
		`{"gps_lat":91,"gps_lon":0}`,
		`{"gps_lat":0,"gps_lon":181}`,
	}
	for _, body := range tests {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/files/"+itoa(file.ID), bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, response = %s", body, w.Code, w.Body.String())
		}
	}

	valid := strings.Repeat("界", maxDescriptionCharacters)
	if utf8.RuneCountInString(valid) != maxDescriptionCharacters {
		t.Fatal("test fixture does not contain 2000 characters")
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/files/"+itoa(file.ID), bytes.NewBufferString(`{"description":"`+valid+`"}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("2000-character description: status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateFileUnknownID(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/files/999", bytes.NewBufferString(`{"description":"missing"}`))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}
