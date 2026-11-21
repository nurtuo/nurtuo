// SPDX-License-Identifier: AGPL-3.0-only

package maps

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Format is the wire format a profile describes.
type Format string

// Supported formats.
const (
	JSON Format = "json"
	XML  Format = "xml"
)

// Type is a field type.
type Type string

// Field types.
const (
	String  Type = "string"
	Number  Type = "number"
	Boolean Type = "boolean"
	Object  Type = "object"
)

// Field is a node in a profile tree. Object fields have Children; Repeated marks
// arrays (JSON) or repeated elements (XML). Attr marks an XML attribute.
type Field struct {
	Name     string
	Type     Type
	Required bool
	Repeated bool
	Attr     bool
	Children []*Field
}

// Profile describes a document structure. Root is the root object (JSON) or the
// root element (XML, Root.Name is the element name).
type Profile struct {
	Name   string
	Format Format
	Root   *Field
}

// Validate checks the profile definition itself.
func (p *Profile) Validate() error {
	if p == nil || p.Root == nil {
		return errors.New("maps: profile has no root")
	}
	if p.Format != JSON && p.Format != XML {
		return fmt.Errorf("maps: profile %q: unknown format %q", p.Name, p.Format)
	}
	if p.Format == XML && p.Root.Name == "" {
		return fmt.Errorf("maps: profile %q: xml root needs a name", p.Name)
	}
	return validateField(p.Root, p.Root.Name)
}

func validateField(f *Field, path string) error {
	switch f.Type {
	case String, Number, Boolean, Object:
	default:
		return fmt.Errorf("maps: field %q: unknown type %q", path, f.Type)
	}
	if f.Type != Object && len(f.Children) > 0 {
		return fmt.Errorf("maps: field %q: only objects have children", path)
	}
	seen := map[string]bool{}
	for _, c := range f.Children {
		if c.Name == "" || seen[c.Name] {
			return fmt.Errorf("maps: field %q: empty or duplicate child name %q", path, c.Name)
		}
		seen[c.Name] = true
		if err := validateField(c, path+"."+c.Name); err != nil {
			return err
		}
	}
	return nil
}

func (f *Field) child(name string) *Field {
	for _, c := range f.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Parse reads a payload into a navigable value (map[string]any, []any, string,
// float64, bool) validated against the profile.
func (p *Profile) Parse(r io.Reader) (any, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Format == JSON {
		return p.parseJSON(r)
	}
	return p.parseXML(r)
}

// ParseBytes is Parse over a byte slice.
func (p *Profile) ParseBytes(b []byte) (any, error) { return p.Parse(bytes.NewReader(b)) }

func (p *Profile) parseJSON(r io.Reader) (any, error) {
	var v any
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return nil, fmt.Errorf("maps: invalid json: %w", err)
	}
	if err := checkValue(p.Root, v, "$"); err != nil {
		return nil, err
	}
	return v, nil
}

func checkValue(f *Field, v any, path string) error {
	if f.Repeated {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("maps: %s: expected array", path)
		}
		for i, e := range arr {
			if err := checkSingle(f, e, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	return checkSingle(f, v, path)
}

func checkSingle(f *Field, v any, path string) error {
	var ok bool
	switch f.Type {
	case String:
		_, ok = v.(string)
	case Number:
		_, ok = v.(float64)
	case Boolean:
		_, ok = v.(bool)
	case Object:
		m, isMap := v.(map[string]any)
		if !isMap {
			return fmt.Errorf("maps: %s: expected object", path)
		}
		for _, c := range f.Children {
			cv, present := m[c.Name]
			if !present || cv == nil {
				if c.Required {
					return fmt.Errorf("maps: %s.%s: required field missing", path, c.Name)
				}
				continue
			}
			if err := checkValue(c, cv, path+"."+c.Name); err != nil {
				return err
			}
		}
		return nil
	}
	if !ok {
		return fmt.Errorf("maps: %s: expected %s", path, f.Type)
	}
	return nil
}

type xmlNode struct {
	name     string
	attrs    []xml.Attr
	children []*xmlNode
	text     strings.Builder
}

func readXML(r io.Reader) (*xmlNode, error) {
	dec := xml.NewDecoder(r)
	var stack []*xmlNode
	var root *xmlNode
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("maps: invalid xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{name: t.Name.Local, attrs: t.Attr}
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.children = append(top.children, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("maps: invalid xml: no root element")
	}
	return root, nil
}

func (p *Profile) parseXML(r io.Reader) (any, error) {
	root, err := readXML(r)
	if err != nil {
		return nil, err
	}
	if root.name != p.Root.Name {
		return nil, fmt.Errorf("maps: xml root is %q, profile expects %q", root.name, p.Root.Name)
	}
	return convertXML(p.Root, root, p.Root.Name)
}

func scalar(t Type, s, path string) (any, error) {
	switch t {
	case Number:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("maps: %s: %q is not a number", path, s)
		}
		return n, nil
	case Boolean:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("maps: %s: %q is not a boolean", path, s)
		}
		return b, nil
	}
	return s, nil
}

func convertXML(f *Field, n *xmlNode, path string) (any, error) {
	if f.Type != Object {
		return scalar(f.Type, n.text.String(), path)
	}
	out := map[string]any{}
	for _, c := range f.Children {
		if c.Attr {
			found := false
			for _, a := range n.attrs {
				if a.Name.Local == c.Name {
					v, err := scalar(c.Type, a.Value, path+"@"+c.Name)
					if err != nil {
						return nil, err
					}
					out[c.Name] = v
					found = true
				}
			}
			if !found && c.Required {
				return nil, fmt.Errorf("maps: %s@%s: required attribute missing", path, c.Name)
			}
			continue
		}
		var matches []*xmlNode
		for _, k := range n.children {
			if k.name == c.Name {
				matches = append(matches, k)
			}
		}
		if len(matches) == 0 {
			if c.Required {
				return nil, fmt.Errorf("maps: %s.%s: required element missing", path, c.Name)
			}
			continue
		}
		if len(matches) > 1 && !c.Repeated {
			return nil, fmt.Errorf("maps: %s.%s: repeated element not declared repeated", path, c.Name)
		}
		vals := make([]any, 0, len(matches))
		for i, m := range matches {
			v, err := convertXML(c, m, fmt.Sprintf("%s.%s[%d]", path, c.Name, i))
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
		if c.Repeated {
			out[c.Name] = vals
		} else {
			out[c.Name] = vals[0]
		}
	}
	return out, nil
}

// Serialize writes a value (as produced by Parse or by map execution) in the
// profile's format.
func (p *Profile) Serialize(v any) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Format == JSON {
		return json.Marshal(v)
	}
	var b bytes.Buffer
	if err := writeXML(&b, p.Root, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeXML(b *bytes.Buffer, f *Field, v any) error {
	if f.Repeated {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("maps: xml %s: expected array", f.Name)
		}
		for _, e := range arr {
			if err := writeXMLOne(b, f, e); err != nil {
				return err
			}
		}
		return nil
	}
	return writeXMLOne(b, f, v)
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeXMLOne(b *bytes.Buffer, f *Field, v any) error {
	if f.Type != Object {
		fmt.Fprintf(b, "<%s>%s</%s>", f.Name, esc(fmt.Sprint(v)), f.Name)
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("maps: xml %s: expected object", f.Name)
	}
	fmt.Fprintf(b, "<%s", f.Name)
	for _, c := range f.Children {
		if cv, ok := m[c.Name]; ok && c.Attr {
			fmt.Fprintf(b, ` %s="%s"`, c.Name, esc(fmt.Sprint(cv)))
		}
	}
	b.WriteByte('>')
	for _, c := range f.Children {
		if c.Attr {
			continue
		}
		if cv, ok := m[c.Name]; ok {
			if err := writeXML(b, c, cv); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(b, "</%s>", f.Name)
	return nil
}
