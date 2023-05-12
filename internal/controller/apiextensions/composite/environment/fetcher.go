/*
Copyright 2022 The Crossplane Authors.

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

package environment

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/pkg/errors"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/resource"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	v1alpha1 "github.com/crossplane/crossplane/apis/apiextensions/v1alpha1"
)

const (
	errGetEnvironmentConfig = "failed to get config set from reference"
	errMergeData            = "failed to merge data"
	errParseWeight          = "failed to parse weight of environment config"

	// AnnotEnvConfigWeight defines order during config aggregation. EnvironmentConfigs with higher weight
	// overwrite common fields of configs with lower weights
	AnnotEnvConfigWeight = "environmentconfig.crossplane.io/weight"

	environmentGroup   = "internal.crossplane.io"
	environmentVersion = "v1alpha1"
	environmentKind    = "Environment"
)

// NewNilEnvironmentFetcher creates a new NilEnvironmentFetcher.
func NewNilEnvironmentFetcher() *NilEnvironmentFetcher {
	return &NilEnvironmentFetcher{}
}

// A NilEnvironmentFetcher always returns nil on Fetch().
type NilEnvironmentFetcher struct{}

// Fetch always returns nil.
func (f *NilEnvironmentFetcher) Fetch(_ context.Context, _ resource.Composite, _ *v1.CompositionRevision) (*Environment, error) {
	return nil, nil
}

// NewAPIEnvironmentFetcher creates a new APIEnvironmentFetcher
func NewAPIEnvironmentFetcher(kube client.Client) *APIEnvironmentFetcher {
	return &APIEnvironmentFetcher{
		kube: kube,
	}
}

// Environment defines unstructured data.
type Environment struct {
	unstructured.Unstructured
}

// APIEnvironmentFetcher fetches the Environments referenced by a composite
// resoruce using a kube client.
type APIEnvironmentFetcher struct {
	kube client.Client
}

// Fetch all EnvironmentConfigs referenced by cr and merge their `.Data` into a
// single Environment.
//
// Note: The `.Data` path is trimmed from the result so its necessary to include
// it in patches.
func (f *APIEnvironmentFetcher) Fetch(ctx context.Context, cr resource.Composite, rev *v1.CompositionRevision) (*Environment, error) {
	var env *Environment

	// Return an empty environment if the XR references no EnvironmentConfigs.
	if len(cr.GetEnvironmentConfigReferences()) == 0 {
		env = &Environment{
			Unstructured: unstructured.Unstructured{
				Object: map[string]interface{}{},
			},
		}
	} else {
		var err error
		var isOptional bool

		if rev != nil && rev.Spec.Environment != nil && rev.Spec.Environment.Policy != nil {
			isOptional = rev.Spec.Environment.Policy.IsResolutionPolicyOptional()
		}

		env, err = f.fetchEnvironment(ctx, cr, isOptional)
		if err != nil {
			return nil, err
		}
	}

	// GVK is necessary for patching because it uses unstructured conversion
	env.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   environmentGroup,
		Version: environmentVersion,
		Kind:    environmentKind,
	})
	return env, nil
}

func getWeight(ec v1alpha1.EnvironmentConfig) (int, error) {

	var w int
	var err error

	if ws, ok := ec.GetAnnotations()[AnnotEnvConfigWeight]; ok {
		w, err = strconv.Atoi(ws)
		if err != nil {
			return -1, err
		}

	} else {
		w = -1
	}
	return w, nil
}

type weightedConfigs struct {
	ec v1alpha1.EnvironmentConfig
	w  int
}

func (f *APIEnvironmentFetcher) fetchEnvironment(ctx context.Context, cr resource.Composite, isOptional bool) (*Environment, error) {
	refs := cr.GetEnvironmentConfigReferences()
	loadedConfigs := []weightedConfigs{}
	for _, ref := range refs {
		config := v1alpha1.EnvironmentConfig{}

		nn := types.NamespacedName{
			Name: ref.Name,
		}
		err := f.kube.Get(ctx, nn, &config)
		if err != nil {
			// skip if resolution policy is optional
			if !isOptional {
				return nil, errors.Wrap(err, errGetEnvironmentConfig)
			}
			continue
		}

		w, err := getWeight(config)

		if err != nil {
			return nil, errors.Wrap(err, errParseWeight)
		}

		loadedConfigs = append(loadedConfigs, weightedConfigs{
			ec: config,
			w:  w,
		})
	}

	// lower weight config will be over written by higher weight config
	sort.Slice(loadedConfigs, func(i, j int) bool {
		return loadedConfigs[i].w < loadedConfigs[j].w
	})

	mergedData, err := mergeEnvironmentData(loadedConfigs)
	if err != nil {
		return nil, errors.Wrap(err, errMergeData)
	}
	return &Environment{
		unstructured.Unstructured{
			Object: mergedData,
		},
	}, nil
}

func mergeEnvironmentData(configs []weightedConfigs) (map[string]interface{}, error) {
	merged := map[string]interface{}{}
	for _, e := range configs {
		if e.ec.Data == nil {
			continue
		}
		data, err := unmarshalData(e.ec.Data)
		if err != nil {
			return nil, err
		}
		merged = mergeMaps(merged, data)
	}
	return merged, nil
}

func unmarshalData(data map[string]extv1.JSON) (map[string]interface{}, error) {
	res := map[string]interface{}{}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// mergeMaps merges b into a.
// Extracted from https://stackoverflow.com/a/70291996
func mergeMaps(a, b map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(a))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if v, ok := v.(map[string]interface{}); ok {
			if bv, ok := out[k]; ok {
				if bv, ok := bv.(map[string]interface{}); ok {
					out[k] = mergeMaps(bv, v)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}
