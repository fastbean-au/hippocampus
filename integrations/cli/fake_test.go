package main

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// fakeClient is a contract.HippocampusClient that records the last request it received and returns
// a canned response (or err). It lets the command handlers be driven without a running service.
type fakeClient struct {
	err error
	req proto.Message // the last request passed to any method

	memoriesResp *contract.GetMemoriesResponse
	whoAmIResp   *contract.WhoAmIResponse
	linksResp    *contract.GetLinksResponse
}

func (f *fakeClient) capture(req proto.Message) { f.req = req }

func (f *fakeClient) Purge(_ context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) Sleep(_ context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) PreviewConsolidation(_ context.Context, in *contract.PreviewConsolidationRequest, _ ...grpc.CallOption) (*contract.PreviewConsolidationResponse, error) {
	f.capture(in)

	return &contract.PreviewConsolidationResponse{}, f.err
}

func (f *fakeClient) GetForgottenMemories(_ context.Context, in *contract.GetForgottenMemoriesRequest, _ ...grpc.CallOption) (*contract.GetForgottenMemoriesResponse, error) {
	f.capture(in)

	return &contract.GetForgottenMemoriesResponse{Enabled: true}, f.err
}

func (f *fakeClient) DeleteForgottenMemories(_ context.Context, in *contract.DeleteForgottenMemoriesRequest, _ ...grpc.CallOption) (*contract.DeleteForgottenMemoriesResponse, error) {
	f.capture(in)

	return &contract.DeleteForgottenMemoriesResponse{}, f.err
}

func (f *fakeClient) ExplainConsolidation(_ context.Context, in *contract.ExplainConsolidationRequest, _ ...grpc.CallOption) (*contract.ExplainConsolidationResponse, error) {
	f.capture(in)

	return &contract.ExplainConsolidationResponse{}, f.err
}

func (f *fakeClient) WhoAmI(_ context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.WhoAmIResponse, error) {
	f.capture(in)

	if f.whoAmIResp != nil {
		return f.whoAmIResp, f.err
	}

	return &contract.WhoAmIResponse{}, f.err
}

func (f *fakeClient) StoreEvent(_ context.Context, in *contract.Event, _ ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	f.capture(in)

	return &contract.StoreEventResponse{Id: "e-new"}, f.err
}

func (f *fakeClient) EndEvent(_ context.Context, in *contract.EndEventRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) UpdateEventSignificance(_ context.Context, in *contract.UpdateEventSignificanceRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) MergeEvents(_ context.Context, in *contract.MergeEventsRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) DeleteEvent(_ context.Context, in *contract.DeleteEventRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) GetEventById(_ context.Context, in *contract.GetEventByIdRequest, _ ...grpc.CallOption) (*contract.GetEventResponse, error) {
	f.capture(in)

	return &contract.GetEventResponse{Event: &contract.Event{Id: in.GetId()}}, f.err
}

func (f *fakeClient) GetEvents(_ context.Context, in *contract.GetEventsRequest, _ ...grpc.CallOption) (*contract.GetEventsResponse, error) {
	f.capture(in)

	return &contract.GetEventsResponse{}, f.err
}

func (f *fakeClient) StoreMemory(_ context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.capture(in)

	return &contract.StoreMemoryResponse{Id: "m-new"}, f.err
}

func (f *fakeClient) UpdateMemory(_ context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) DeleteMemories(_ context.Context, in *contract.DeleteMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) GetMemories(_ context.Context, in *contract.GetMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.capture(in)

	if f.memoriesResp != nil {
		return f.memoriesResp, f.err
	}

	return &contract.GetMemoriesResponse{}, f.err
}

func (f *fakeClient) RecallMemories(_ context.Context, in *contract.RecallMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.capture(in)

	return &contract.GetMemoriesResponse{}, f.err
}

func (f *fakeClient) SearchMemories(_ context.Context, in *contract.SearchMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.capture(in)

	return &contract.GetMemoriesResponse{}, f.err
}

func (f *fakeClient) ReplaceMemoriesWithSummary(_ context.Context, in *contract.ReplaceMemoriesWithSummaryRequest, _ ...grpc.CallOption) (*contract.ReplaceMemoriesWithSummaryResponse, error) {
	f.capture(in)

	return &contract.ReplaceMemoriesWithSummaryResponse{Id: "s-new"}, f.err
}

func (f *fakeClient) GetSummarisationCandidates(_ context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GetSummarisationCandidatesResponse, error) {
	f.capture(in)

	return &contract.GetSummarisationCandidatesResponse{}, f.err
}

func (f *fakeClient) SummariseMemories(_ context.Context, in *contract.SummariseMemoriesRequest, _ ...grpc.CallOption) (*contract.SummariseMemoriesResponse, error) {
	f.capture(in)

	return &contract.SummariseMemoriesResponse{Id: "s-new"}, f.err
}

func (f *fakeClient) Export(_ context.Context, in *contract.ExportRequest, _ ...grpc.CallOption) (*contract.ExportResponse, error) {
	f.capture(in)

	return &contract.ExportResponse{ManifestId: "man-1"}, f.err
}

func (f *fakeClient) Import(_ context.Context, in *contract.ImportRequest, _ ...grpc.CallOption) (*contract.ImportResponse, error) {
	f.capture(in)

	return &contract.ImportResponse{}, f.err
}

func (f *fakeClient) ImportBatch(_ context.Context, in *contract.ImportBatchRequest, _ ...grpc.CallOption) (*contract.ImportBatchResponse, error) {
	f.capture(in)

	return &contract.ImportBatchResponse{}, f.err
}

func (f *fakeClient) Transfer(_ context.Context, in *contract.TransferRequest, _ ...grpc.CallOption) (*contract.TransferResponse, error) {
	f.capture(in)

	return &contract.TransferResponse{ManifestId: "man-2"}, f.err
}

func (f *fakeClient) Clear(_ context.Context, in *contract.ClearRequest, _ ...grpc.CallOption) (*contract.ClearResponse, error) {
	f.capture(in)

	return &contract.ClearResponse{}, f.err
}

func (f *fakeClient) LinkMemories(_ context.Context, in *contract.LinkMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) UnlinkMemories(_ context.Context, in *contract.UnlinkMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) GetMemoryLinks(_ context.Context, in *contract.GetMemoryLinksRequest, _ ...grpc.CallOption) (*contract.GetLinksResponse, error) {
	f.capture(in)

	if f.linksResp != nil {
		return f.linksResp, f.err
	}

	return &contract.GetLinksResponse{}, f.err
}

func (f *fakeClient) LinkEvents(_ context.Context, in *contract.LinkEventsRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) UnlinkEvents(_ context.Context, in *contract.UnlinkEventsRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.capture(in)

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) GetEventLinks(_ context.Context, in *contract.GetEventLinksRequest, _ ...grpc.CallOption) (*contract.GetLinksResponse, error) {
	f.capture(in)

	if f.linksResp != nil {
		return f.linksResp, f.err
	}

	return &contract.GetLinksResponse{}, f.err
}
