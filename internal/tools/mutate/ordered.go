package main

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// An order-preserving JSON object.
//
// The manifest is historical evidence, and this tool's job is to add a result to
// it rather than to reshape it. Go's encoding/json marshals a map with its keys
// sorted, so decoding the manifest into maps and writing it back would reorder
// every field of every class — a diff touching all forty-six entries, in which a
// real change to one of them would be invisible.
//
// So an object keeps the order it was read in, and a key added later is appended
// rather than sorted into the middle.

type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object {
	return &object{vals: map[string]json.RawMessage{}}
}

func (o *object) UnmarshalJSON(b []byte) error {
	o.keys = nil
	o.vals = map[string]json.RawMessage{}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected a JSON object, got %v", tok)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("expected an object key, got %v", tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if _, seen := o.vals[key]; !seen {
			o.keys = append(o.keys, key)
		}
		o.vals[key] = raw
	}
	_, err = dec.Token() // closing }
	return err
}

func (o object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (o *object) get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *object) setRaw(key string, raw json.RawMessage) {
	if _, seen := o.vals[key]; !seen {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

// set marshals a value and stores it, keeping the key's position if it has one.
func (o *object) set(key string, value any) error {
	b, err := marshal(value)
	if err != nil {
		return err
	}
	o.setRaw(key, b)
	return nil
}

// marshal encodes without HTML escaping, so an em dash or an angle bracket in a
// comment stays what it is rather than becoming an escape sequence.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// indent renders a document with two-space indentation, the shape the manifest
// already has.
func indent(v any) ([]byte, error) {
	raw, err := marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}
