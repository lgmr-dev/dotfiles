package hooks

import "github.com/lgmr-dev/dotfiles/installer/targets"

type terminalReadKey struct {
	start  *byte
	length int
}

func terminalKeyOf(data []byte) terminalReadKey {
	key := terminalReadKey{length: len(data)}
	if len(data) != 0 {
		key.start = &data[0]
	}
	return key
}

type terminalReadArray struct {
	elements []terminalElement
	lexical  []byte
}

type terminalReader struct {
	properties map[terminalReadKey][]targets.JSONProperty
	arrays     map[terminalReadKey]terminalReadArray
}

func (r *terminalReader) propertiesOf(data []byte) ([]targets.JSONProperty, error) {
	key := terminalKeyOf(data)
	if properties, ok := r.properties[key]; ok {
		return properties, nil
	}
	properties, err := terminalProperties(data)
	if err != nil {
		return nil, err
	}
	if r.properties == nil {
		r.properties = make(map[terminalReadKey][]targets.JSONProperty)
	}
	r.properties[key] = properties
	return properties, nil
}

func (r *terminalReader) elementsOf(data []byte) ([]terminalElement, []byte, error) {
	key := terminalKeyOf(data)
	if array, ok := r.arrays[key]; ok {
		return array.elements, array.lexical, nil
	}
	elements, lexical, err := terminalElements(data)
	if err != nil {
		return nil, nil, err
	}
	if r.arrays == nil {
		r.arrays = make(map[terminalReadKey]terminalReadArray)
	}
	r.arrays[key] = terminalReadArray{elements, lexical}
	return elements, lexical, nil
}

func (r *terminalReader) get(data []byte, path string) ([]byte, bool, error) {
	return terminalLookup(data, path, r.propertiesOf)
}

func (r *terminalReader) entry(array []byte, collection terminalCollection) ([]byte, int, error) {
	return terminalFindEntry(array, collection, r.elementsOf, r.get)
}
