// Copyright 2024 EMQ Technologies Co., Ltd.
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

package tracer

import (
	"encoding/json"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type LocalSpan struct {
	Name         string                 `json:"name"`
	TraceID      string                 `json:"traceID"`
	SpanID       string                 `json:"spanID"`
	ParentSpanID string                 `json:"parentSpanID,omitempty"`
	Attribute    map[string]interface{} `json:"attribute,omitempty"`
	Links        []LocalLink            `json:"links,omitempty"`
	StartTime    time.Time              `json:"startTime"`
	EndTime      time.Time              `json:"endTime"`
	RuleID       string                 `json:"ruleID"`

	ChildSpan []*LocalSpan
}

type LocalLink struct {
	TraceID string `yaml:"traceID"`
}

func (span *LocalSpan) ToBytes() ([]byte, error) {
	return json.Marshal(span)
}

func FromReadonlySpan(readonly sdktrace.ReadOnlySpan) *LocalSpan {
	span := &LocalSpan{
		Name:         readonly.Name(),
		TraceID:      readonly.SpanContext().TraceID().String(),
		SpanID:       readonly.SpanContext().SpanID().String(),
		ParentSpanID: readonly.Parent().SpanID().String(),
		ChildSpan:    make([]*LocalSpan, 0),
		StartTime:    readonly.StartTime(),
		EndTime:      readonly.EndTime(),
	}
	if len(readonly.Attributes()) > 0 {
		span.Attribute = make(map[string]interface{})
		for _, attr := range readonly.Attributes() {
			if string(attr.Key) == "rule" {
				span.RuleID = attr.Value.AsString()
			}
			span.Attribute[string(attr.Key)] = attr.Value.AsInterface()
		}
	}
	if len(readonly.Links()) > 0 {
		span.Links = make([]LocalLink, 0)
		for _, link := range readonly.Links() {
			span.Links = append(span.Links, LocalLink{
				TraceID: link.SpanContext.TraceID().String(),
			})
		}
	}
	return span
}