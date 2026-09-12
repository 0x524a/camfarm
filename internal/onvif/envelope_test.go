// internal/onvif/envelope_test.go
package onvif

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseActionSOAP11(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
  <soap:Body>
    <GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl">
      <ProfileToken>profile1</ProfileToken>
    </GetStreamUri>
  </soap:Body>
</soap:Envelope>`)

	name, payload, err := parseAction(body)
	if err != nil {
		t.Fatalf("parseAction: %v", err)
	}
	if name != "GetStreamUri" {
		t.Errorf("name = %q, want GetStreamUri", name)
	}
	if !strings.Contains(string(payload), "profile1") {
		t.Errorf("payload = %q, want it to contain profile1", payload)
	}
}

func TestParseActionSOAP12(t *testing.T) {
	body := []byte(`<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope">
  <soap:Body>
    <GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/>
  </soap:Body>
</soap:Envelope>`)

	name, _, err := parseAction(body)
	if err != nil {
		t.Fatalf("parseAction: %v", err)
	}
	if name != "GetDeviceInformation" {
		t.Errorf("name = %q, want GetDeviceInformation", name)
	}
}

func TestParseActionMalformed(t *testing.T) {
	if _, _, err := parseAction([]byte("not xml")); err == nil {
		t.Fatal("parseAction: want error for malformed input")
	}
}

func TestWriteResponseWrapsContent(t *testing.T) {
	type stub struct {
		XMLName struct{} `xml:"http://example.com/wsdl StubResponse"`
		Value   string   `xml:"Value"`
	}

	rec := httptest.NewRecorder()
	writeResponse(rec, &stub{Value: "hello"})

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<soap:Envelope") || !strings.Contains(body, "StubResponse") || !strings.Contains(body, "hello") {
		t.Errorf("body = %q, missing envelope/content", body)
	}
}

func TestWriteFaultShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFault(rec, "Client", "Action not supported")

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 (a fault envelope, not a bare HTTP error)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "soap:Fault") || !strings.Contains(body, "soap:Client") || !strings.Contains(body, "Action not supported") {
		t.Errorf("body = %q, missing fault shape", body)
	}
}
