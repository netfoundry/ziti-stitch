/*
	Copyright 2026 NetFoundry Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/openziti/identity"
)

// pemPrefix marks inline PEM in an identity file, as opposed to a file path.
const pemPrefix = "pem:"

// setControllerURLs returns the identity file content with ztAPIs set to
// urls, and whether that changed anything. Other fields are kept as they
// were (key order aside).
func setControllerURLs(raw []byte, urls []string) ([]byte, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("parse identity file: %w", err)
	}
	var current []string
	if v, ok := doc["ztAPIs"]; ok {
		// null and a missing field both mean "no list yet".
		if err := json.Unmarshal(v, &current); err != nil {
			return nil, false, fmt.Errorf("parse ztAPIs: %w", err)
		}
	}
	if slices.Equal(current, urls) {
		return raw, false, nil
	}
	v, err := json.Marshal(urls)
	if err != nil {
		return nil, false, err
	}
	doc["ztAPIs"] = v
	out, err := json.MarshalIndent(doc, "", "  ")
	return out, true, err
}

// replaceCredentials returns the identity file content with the key and
// certificate replaced by keyPEM and certPEM, and the CA bundle by caPEM
// unless caPEM is empty.
func replaceCredentials(raw []byte, keyPEM, certPEM, caPEM string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse identity file: %w", err)
	}
	var id map[string]json.RawMessage
	if err := json.Unmarshal(doc["id"], &id); err != nil || id == nil {
		return nil, fmt.Errorf("identity file has no id object")
	}
	set := func(k, pem string) error {
		v, err := json.Marshal(pemPrefix + pem)
		id[k] = v
		return err
	}
	if err := set("key", keyPEM); err != nil {
		return nil, err
	}
	if err := set("cert", certPEM); err != nil {
		return nil, err
	}
	if caPEM != "" {
		if err := set("ca", caPEM); err != nil {
			return nil, err
		}
	}
	v, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	doc["id"] = v
	return json.MarshalIndent(doc, "", "  ")
}

// credentials returns the key, certificate and CA entries of an identity
// file, exactly as stored (so inline PEM keeps its "pem:" prefix).
func credentials(raw []byte) (identity.Config, error) {
	var doc struct {
		ID *identity.Config `json:"id"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return identity.Config{}, fmt.Errorf("parse identity file: %w", err)
	}
	if doc.ID == nil || doc.ID.Key == "" || doc.ID.Cert == "" {
		return identity.Config{}, errors.New("identity file has no key or certificate")
	}
	return *doc.ID, nil
}
