/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package flag

import (
	"fmt"
	"sort"
	"strings"
	"strconv"
)

// MapStringInts can be set from the command line with the format string `--flag "string=int1,int2,..."`, or
// `--flag "string=int1-int2"`. Multiple comma-separated key-value pairs in a single invocation are supported.
// For example: `--flag "a=1-3,b=4,5"`.
// Multiple flag invocations are supported. For example: `--flag "a=1-3" --flag "b=4,5"`.
type MapStringInts struct {
	Map         *map[string][]int
	initialized bool
}

// NewMapStringInts takes a pointer to a map[string][]int and returns the
// MapStringInts flag parsing shim for that map
func NewMapStringInts(m *map[string][]int) *MapStringInts {
	return &MapStringInts{Map: m}
}

// String implements github.com/spf13/pflag.Value
func (m *MapStringInts) String() string {
	pairs := []string{}
	for k, v := range *m.Map {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// Set implements github.com/spf13/pflag.Value
func (m *MapStringInts) Set(value string) error {
	if m.Map == nil {
		return fmt.Errorf("no target (nil pointer to map[string][]int)")
	}
	if !m.initialized || *m.Map == nil {
		// clear default values, or allocate if no existing map
		*m.Map = make(map[string][]int)
		m.initialized = true
	}
	
	for eq := strings.LastIndex(value, "="); eq != -1; eq = strings.LastIndex(value, "=") {
		b := strings.LastIndex(value[0:eq], ",")

		if err := m.parseOne(value[b+1:eq], value[eq+1:]); err != nil {
			return err
		}

		if b < 0 {
			break
		}

		value = value[0:b]
	}
	return nil
}

// Type implements github.com/spf13/pflag.Value
func (*MapStringInts) Type() string {
	return "mapStringInts"
}

// Empty implements OmitEmpty
func (m *MapStringInts) Empty() bool {
	return len(*m.Map) == 0
}

// Parse a single name=values entry which can be a mix of comma-separated single values, or
// inclusive ranges of the form 'from-to'.
func (m *MapStringInts) parseOne(key, value string) error {
	if v, ok := (*m.Map)[key]; ok {
		return fmt.Errorf("key %s has already values set (to %v)", key, v)
	}
	
	values := []int{}

	for _, v := range strings.Split(value, ",") {
		if len(v) == 0 {
			continue
		}

		var from, to int64
		var err error

		if dash := strings.LastIndex(v, "-"); dash != -1 {
			rng := strings.Split(v, "-")

			if len(rng) != 2 {
				return fmt.Errorf("invalid range (%s) for key '%s' in value '%s'", v, key, value)
			}
			if from, err = strconv.ParseInt(rng[0], 10, 0); err != nil {
				return fmt.Errorf("invalid range (%s) for key '%s' in value '%s'", v, key, value)
			}
			if to, err = strconv.ParseInt(rng[1], 10, 0); err != nil {
				return fmt.Errorf("invalid range (%s) for key '%s' in value '%s'", v, key, value)
			}
			if from >= to {
				return fmt.Errorf("invalid range (%s) for key '%s' in value '%s'", v, key, value)
			}
		} else {
			if from, err = strconv.ParseInt(v, 10, 0); err != nil {
				return fmt.Errorf("invalid value (%s) for key '%s' in value '%s'", v, key, value)
			}

			to = from
		}

		for i := from; i <= to; i++ {
			values = append(values, int(i))
		}
	}

	sort.Ints(values)
	(*m.Map)[key] = values

	return nil
}
