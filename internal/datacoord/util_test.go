// licensed to the lf ai & data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datacoord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	"github.com/milvus-io/milvus/internal/common"
	"github.com/milvus-io/milvus/internal/proto/rootcoordpb"
	"github.com/milvus-io/milvus/internal/util/paramtable"
	"github.com/milvus-io/milvus/internal/util/tsoutil"
	"github.com/stretchr/testify/suite"
)

type UtilSuite struct {
	suite.Suite
}

func (suite *UtilSuite) TestVerifyResponse() {
	type testCase struct {
		resp       interface{}
		err        error
		expected   error
		equalValue bool
	}
	cases := []testCase{
		{
			resp:       nil,
			err:        errors.New("boom"),
			expected:   errors.New("boom"),
			equalValue: true,
		},
		{
			resp:       nil,
			err:        nil,
			expected:   errNilResponse,
			equalValue: false,
		},
		{
			resp:       &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success},
			err:        nil,
			expected:   nil,
			equalValue: false,
		},
		{
			resp:       &commonpb.Status{ErrorCode: commonpb.ErrorCode_UnexpectedError, Reason: "r1"},
			err:        nil,
			expected:   errors.New("r1"),
			equalValue: true,
		},
		{
			resp:       (*commonpb.Status)(nil),
			err:        nil,
			expected:   errNilResponse,
			equalValue: false,
		},
		{
			resp: &rootcoordpb.AllocIDResponse{
				Status: &commonpb.Status{ErrorCode: commonpb.ErrorCode_Success},
			},
			err:        nil,
			expected:   nil,
			equalValue: false,
		},
		{
			resp: &rootcoordpb.AllocIDResponse{
				Status: &commonpb.Status{ErrorCode: commonpb.ErrorCode_UnexpectedError, Reason: "r2"},
			},
			err:        nil,
			expected:   errors.New("r2"),
			equalValue: true,
		},
		{
			resp:       &rootcoordpb.AllocIDResponse{},
			err:        nil,
			expected:   errNilStatusResponse,
			equalValue: true,
		},
		{
			resp:       (*rootcoordpb.AllocIDResponse)(nil),
			err:        nil,
			expected:   errNilStatusResponse,
			equalValue: true,
		},
		{
			resp:       struct{}{},
			err:        nil,
			expected:   errUnknownResponseType,
			equalValue: false,
		},
	}
	for _, c := range cases {
		r := VerifyResponse(c.resp, c.err)
		if c.equalValue {
			suite.EqualValues(c.expected, r)
		} else {
			suite.Equal(c.expected, r)
		}
	}
}

func (suite *UtilSuite) TestGetCompactTime() {
	paramtable.Get().Save(Params.CommonCfg.RetentionDuration.Key, "43200") // 5 days
	defer paramtable.Get().Reset(Params.CommonCfg.RetentionDuration.Key)   // 5 days

	tFixed := time.Date(2021, 11, 15, 0, 0, 0, 0, time.Local)
	tBefore := tFixed.Add(-1 * Params.CommonCfg.RetentionDuration.GetAsDuration(time.Second))

	type args struct {
		allocator allocator
	}
	tests := []struct {
		name    string
		args    args
		want    *compactTime
		wantErr bool
	}{
		{
			"test get timetravel",
			args{&fixedTSOAllocator{fixedTime: tFixed}},
			&compactTime{tsoutil.ComposeTS(tBefore.UnixNano()/int64(time.Millisecond), 0), 0, 0},
			false,
		},
	}
	for _, tt := range tests {
		suite.Run(tt.name, func() {
			got, err := GetCompactTime(context.TODO(), tt.args.allocator)
			suite.Equal(tt.wantErr, err != nil)
			suite.EqualValues(tt.want, got)
		})
	}
}

func TestUtil(t *testing.T) {
	suite.Run(t, new(UtilSuite))
}

type fixedTSOAllocator struct {
	fixedTime time.Time
}

func (f *fixedTSOAllocator) allocTimestamp(_ context.Context) (Timestamp, error) {
	return tsoutil.ComposeTS(f.fixedTime.UnixNano()/int64(time.Millisecond), 0), nil
}

func (f *fixedTSOAllocator) allocID(_ context.Context) (UniqueID, error) {
	panic("not implemented") // TODO: Implement
}

func (suite *UtilSuite) TestGetZeroTime() {
	n := 10
	for i := 0; i < n; i++ {
		timeGot := getZeroTime()
		suite.True(timeGot.IsZero())
	}
}

func (suite *UtilSuite) TestGetCollectionTTL() {
	properties1 := map[string]string{
		common.CollectionTTLConfigKey: "3600",
	}

	// get ttl from configuration file
	ttl, err := getCollectionTTL(properties1)
	suite.NoError(err)
	suite.Equal(ttl, time.Duration(3600)*time.Second)

	properties2 := map[string]string{
		common.CollectionTTLConfigKey: "error value",
	}
	// test for parsing configuration failed
	ttl, err = getCollectionTTL(properties2)
	suite.Error(err)
	suite.Equal(int(ttl), -1)

	ttl, err = getCollectionTTL(map[string]string{})
	suite.NoError(err)
	suite.Equal(ttl, Params.CommonCfg.EntityExpirationTTL.GetAsDuration(time.Second))
}
