package onvif

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
)

const soapContentType = `application/soap+xml; charset=utf-8`

// parseAction extracts the SOAP body's first child element's local name (the
// action) and that element's own XML, re-wrapped under its local name with
// its inner content untouched, by tokenizing rather than unmarshaling into a
// namespace-pinned struct. Matching only the local name is what lets this
// accept SOAP 1.1 and SOAP 1.2 envelopes alike, and is why this package owns
// its own transport instead of reusing onvif-go's client-side Envelope type.
// The wrapping tag matters: onvif-go's request structs (e.g. the one behind
// HandleGetStreamURI) have no XMLName and expect fields like ProfileToken as
// a *child* of the root element, so passing the action's inner XML alone —
// with ProfileToken itself as the root — would leave those fields unset.
func parseAction(soapBody []byte) (name string, payload []byte, err error) {
	dec := xml.NewDecoder(bytes.NewReader(soapBody))
	depth := 0
	bodyDepth := -1

	for {
		tok, terr := dec.Token()
		if terr != nil {
			return "", nil, fmt.Errorf("onvif: no SOAP action found: %w", terr)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "Body" {
				bodyDepth = depth
				continue
			}
			if bodyDepth != -1 && depth == bodyDepth+1 {
				var raw struct {
					Inner []byte `xml:",innerxml"`
				}
				if err := dec.DecodeElement(&raw, &t); err != nil {
					return "", nil, fmt.Errorf("onvif: decoding action %q: %w", t.Name.Local, err)
				}
				outer := fmt.Sprintf("<%s>%s</%s>", t.Name.Local, raw.Inner, t.Name.Local)
				return t.Name.Local, []byte(outer), nil
			}
		case xml.EndElement:
			depth--
		}
	}
}

// writeResponse marshals content (one of onvif-go's own *XxxResponse types)
// and wraps it in a hand-written SOAP 1.1 envelope.
//
// The envelope is written by hand rather than through encoding/xml:
// marshaling an outer struct whose elements need the literal "soap:" prefix
// real ONVIF clients expect fights encoding/xml's own prefix generation, and
// content's own XMLName tag already marshals correctly on its own — the two
// concerns don't need to share one call.
func writeResponse(w http.ResponseWriter, content any) {
	inner, err := xml.Marshal(content)
	if err != nil {
		writeFault(w, "Server", "failed to marshal response")
		return
	}
	w.Header().Set("Content-Type", soapContentType)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, xml.Header+
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">`+
		`<soap:Body>%s</soap:Body></soap:Envelope>`, inner)
}

// writeFault sends a SOAP fault envelope. Always HTTP 200: a real ONVIF
// client expects a fault envelope on the wire, and a bare HTTP error code is
// not legible to it as "not supported" the way a fault code is.
func writeFault(w http.ResponseWriter, code, reason string) {
	w.Header().Set("Content-Type", soapContentType)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, xml.Header+
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">`+
		`<soap:Body><soap:Fault><faultcode>soap:%s</faultcode><faultstring>%s</faultstring></soap:Fault></soap:Body></soap:Envelope>`,
		code, xmlEscape(reason))
}

func xmlEscape(s string) string {
	var buf strings.Builder
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
