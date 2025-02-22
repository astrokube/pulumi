// Copyright 2016-2022, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httpstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	opentracing "github.com/opentracing/opentracing-go"
)

type deploymentDiffState struct {
	lastSavedDeployment json.RawMessage
	sequenceNumber      int
	noChecksums         bool
	strictMode          bool
	minimalDiffSize     int
}

type deploymentDiff struct {
	sequenceNumber  int
	checkpointHash  string
	deploymentDelta json.RawMessage
}

func newDeploymentDiffState() *deploymentDiffState {
	return &deploymentDiffState{sequenceNumber: 1}
}

func (dds *deploymentDiffState) SequenceNumber() int {
	return dds.sequenceNumber
}

func (dds *deploymentDiffState) CanDiff() bool {
	return dds.lastSavedDeployment != nil
}

// Size-based heuristics trying to estimate if the diff method will be
// worth it and take less time than sending the entire deployment.
func (dds *deploymentDiffState) ShouldDiff(new *apitype.UntypedDeployment) bool {
	if !dds.CanDiff() {
		return false
	}
	small := dds.minimalDiffSize
	if small == 0 {
		small = 1024 * 32
	}
	if len(dds.lastSavedDeployment) < small {
		return false
	}
	if len(new.Deployment) < small {
		return false
	}
	return true
}

func (dds *deploymentDiffState) Diff(ctx context.Context,
	deployment *apitype.UntypedDeployment) (deploymentDiff, error) {

	if !dds.CanDiff() {
		return deploymentDiff{}, fmt.Errorf("Diff() cannot be called before Saved()")
	}

	if deployment.Version == 0 {
		return deploymentDiff{}, fmt.Errorf("deployment.Version should be set")
	}

	tracingSpan, childCtx := opentracing.StartSpanFromContext(ctx, "Diff")
	defer tracingSpan.Finish()

	before := dds.lastSavedDeployment

	after, err := marshalUntypedDeployment(deployment)
	if err != nil {
		return deploymentDiff{}, fmt.Errorf("marshalUntypedDeployment failed: %v", err)
	}

	var checkpointHash string
	checkpointHashReady := &sync.WaitGroup{}

	if !dds.noChecksums {
		checkpointHashReady.Add(1)
		go func() {
			defer checkpointHashReady.Done()
			checkpointHash = dds.computeHash(childCtx, after)
		}()
	}

	delta, err := dds.computeEdits(childCtx, string(before), string(after))
	if err != nil {
		return deploymentDiff{}, fmt.Errorf("Cannot marshal the edits: %v", err)
	}

	checkpointHashReady.Wait()

	tracingSpan.SetTag("before", len(before))
	tracingSpan.SetTag("after", len(after))
	tracingSpan.SetTag("diff", len(delta))
	tracingSpan.SetTag("compression", 100.0*float64(len(delta))/float64(len(after)))
	tracingSpan.SetTag("hash", checkpointHash)

	diff := deploymentDiff{
		checkpointHash:  checkpointHash,
		deploymentDelta: delta,
		sequenceNumber:  dds.sequenceNumber,
	}

	return diff, nil
}

// Indicates that a deployment was just saved to the service.
func (dds *deploymentDiffState) Saved(ctx context.Context, deployment *apitype.UntypedDeployment) error {
	d, err := marshalUntypedDeployment(deployment)
	if err != nil {
		return err
	}
	dds.lastSavedDeployment = d
	dds.sequenceNumber++

	return nil
}

func (*deploymentDiffState) computeHash(ctx context.Context, deployment json.RawMessage) string {
	tracingSpan, _ := opentracing.StartSpanFromContext(ctx, "computeHash")
	defer tracingSpan.Finish()
	hash := sha256.Sum256(deployment)
	return hex.EncodeToString(hash[:])
}

func (*deploymentDiffState) computeEdits(ctx context.Context, before, after string) (json.RawMessage, error) {
	tracingSpan, _ := opentracing.StartSpanFromContext(ctx, "computeEdits")
	defer tracingSpan.Finish()

	edits := myers.ComputeEdits(span.URIFromURI(""), before, after)

	delta, err := json.Marshal(edits)
	if err != nil {
		return nil, fmt.Errorf("Cannot marshal the edits: %v", err)
	}

	return delta, nil
}
