package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// httpClient speaks to the service's /v1 grpc-gateway, implementing the same generated
// contract.HippocampusClient interface as the native gRPC client so command handlers do not care
// which transport they are given. Each method maps its RPC onto the gateway's HTTP method, path,
// and body binding exactly as the google.api.http annotations in hippocampus.proto declare them;
// requests and responses are (un)marshalled with protojson so field naming matches the gateway.
type httpClient struct {
	baseURL string
	token   string
	http    *http.Client
}

var _ contract.HippocampusClient = (*httpClient)(nil)

func (c *httpClient) Purge(ctx context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/purge", nil, nil, out)
}

func (c *httpClient) Sleep(ctx context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/sleep", nil, nil, out)
}

func (c *httpClient) WhoAmI(ctx context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.WhoAmIResponse, error) {
	out := &contract.WhoAmIResponse{}

	return out, c.do(ctx, http.MethodGet, "/v1/whoami", nil, nil, out)
}

func (c *httpClient) GetTopology(ctx context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GetTopologyResponse, error) {
	out := &contract.GetTopologyResponse{}

	return out, c.do(ctx, http.MethodGet, "/v1/topology", nil, nil, out)
}

func (c *httpClient) GetConsolidationStatus(ctx context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GetConsolidationStatusResponse, error) {
	out := &contract.GetConsolidationStatusResponse{}

	return out, c.do(ctx, http.MethodGet, "/v1/consolidation/status", nil, nil, out)
}

func (c *httpClient) StoreEvent(ctx context.Context, in *contract.Event, _ ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	out := &contract.StoreEventResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/events", nil, in, out)
}

func (c *httpClient) EndEvent(ctx context.Context, in *contract.EndEventRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/events/"+pathSegment(in.GetId())+"/end", nil, in, out)
}

func (c *httpClient) UpdateEventSignificance(ctx context.Context, in *contract.UpdateEventSignificanceRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPatch, "/v1/events/"+pathSegment(in.GetId())+"/significance", nil, in, out)
}

func (c *httpClient) MergeEvents(ctx context.Context, in *contract.MergeEventsRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/events/merge", nil, in, out)
}

func (c *httpClient) DeleteEvent(ctx context.Context, in *contract.DeleteEventRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.doQuery(ctx, http.MethodDelete, "/v1/events/"+pathSegment(in.GetId()), in, map[string]bool{"id": true}, out)
}

func (c *httpClient) GetEventById(ctx context.Context, in *contract.GetEventByIdRequest, _ ...grpc.CallOption) (*contract.GetEventResponse, error) {
	out := &contract.GetEventResponse{}

	return out, c.doQuery(ctx, http.MethodGet, "/v1/events/"+pathSegment(in.GetId()), in, map[string]bool{"id": true}, out)
}

func (c *httpClient) GetEvents(ctx context.Context, in *contract.GetEventsRequest, _ ...grpc.CallOption) (*contract.GetEventsResponse, error) {
	out := &contract.GetEventsResponse{}

	return out, c.doQuery(ctx, http.MethodGet, "/v1/events", in, nil, out)
}

func (c *httpClient) StoreMemory(ctx context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	out := &contract.StoreMemoryResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/memories", nil, in, out)
}

func (c *httpClient) UpdateMemory(ctx context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPatch, "/v1/memories/"+pathSegment(in.GetId()), nil, in, out)
}

func (c *httpClient) DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/memories/delete", nil, in, out)
}

func (c *httpClient) GetMemories(ctx context.Context, in *contract.GetMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	out := &contract.GetMemoriesResponse{}

	return out, c.doQuery(ctx, http.MethodGet, "/v1/memories", in, nil, out)
}

func (c *httpClient) PreviewConsolidation(ctx context.Context, in *contract.PreviewConsolidationRequest, _ ...grpc.CallOption) (*contract.PreviewConsolidationResponse, error) {
	out := &contract.PreviewConsolidationResponse{}

	return out, c.doQuery(ctx, http.MethodGet, "/v1/sleep/preview", in, nil, out)
}

func (c *httpClient) GetForgottenMemories(ctx context.Context, in *contract.GetForgottenMemoriesRequest, _ ...grpc.CallOption) (*contract.GetForgottenMemoriesResponse, error) {
	out := &contract.GetForgottenMemoriesResponse{}

	return out, c.doQuery(ctx, http.MethodGet, "/v1/memories/forgotten", in, nil, out)
}

func (c *httpClient) DeleteForgottenMemories(ctx context.Context, in *contract.DeleteForgottenMemoriesRequest, _ ...grpc.CallOption) (*contract.DeleteForgottenMemoriesResponse, error) {
	out := &contract.DeleteForgottenMemoriesResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/memories/forgotten/delete", nil, in, out)
}

func (c *httpClient) ExplainConsolidation(ctx context.Context, in *contract.ExplainConsolidationRequest, _ ...grpc.CallOption) (*contract.ExplainConsolidationResponse, error) {
	out := &contract.ExplainConsolidationResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/consolidation/explain", nil, in, out)
}

func (c *httpClient) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	out := &contract.GetMemoriesResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/memories/recall", nil, in, out)
}

func (c *httpClient) SearchMemories(ctx context.Context, in *contract.SearchMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	out := &contract.GetMemoriesResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/memories/search", nil, in, out)
}

// The link surface. Each maps onto the gateway binding its google.api.http annotation declares:
// the near end is a path segment, and the rest of the message is the body (or, for the reads, the
// query string).

func (c *httpClient) LinkMemories(ctx context.Context, in *contract.LinkMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/memories/"+pathSegment(in.GetId())+"/links", nil, in, out)
}

func (c *httpClient) UnlinkMemories(ctx context.Context, in *contract.UnlinkMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/memories/"+pathSegment(in.GetId())+"/links/delete", nil, in, out)
}

func (c *httpClient) GetMemoryLinks(ctx context.Context, in *contract.GetMemoryLinksRequest, _ ...grpc.CallOption) (*contract.GetLinksResponse, error) {
	out := &contract.GetLinksResponse{}

	return out, c.doQuery(ctx, http.MethodGet, "/v1/memories/"+pathSegment(in.GetId())+"/links", in, map[string]bool{"id": true}, out)
}

func (c *httpClient) LinkEvents(ctx context.Context, in *contract.LinkEventsRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/events/"+pathSegment(in.GetId())+"/links", nil, in, out)
}

func (c *httpClient) UnlinkEvents(ctx context.Context, in *contract.UnlinkEventsRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	out := &contract.GeneralResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/events/"+pathSegment(in.GetId())+"/links/delete", nil, in, out)
}

func (c *httpClient) GetEventLinks(ctx context.Context, in *contract.GetEventLinksRequest, _ ...grpc.CallOption) (*contract.GetLinksResponse, error) {
	out := &contract.GetLinksResponse{}

	return out, c.doQuery(ctx, http.MethodGet, "/v1/events/"+pathSegment(in.GetId())+"/links", in, map[string]bool{"id": true}, out)
}

func (c *httpClient) ReplaceMemoriesWithSummary(ctx context.Context, in *contract.ReplaceMemoriesWithSummaryRequest, _ ...grpc.CallOption) (*contract.ReplaceMemoriesWithSummaryResponse, error) {
	out := &contract.ReplaceMemoriesWithSummaryResponse{}

	// The annotation binds only the `summary` field to the body; event_id travels in the path.
	return out, c.do(ctx, http.MethodPost, "/v1/events/"+pathSegment(in.GetEventId())+"/summary", nil, in.GetSummary(), out)
}

func (c *httpClient) GetSummarisationCandidates(ctx context.Context, in *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GetSummarisationCandidatesResponse, error) {
	out := &contract.GetSummarisationCandidatesResponse{}

	return out, c.do(ctx, http.MethodGet, "/v1/summarisation/candidates", nil, nil, out)
}

func (c *httpClient) SummariseMemories(ctx context.Context, in *contract.SummariseMemoriesRequest, _ ...grpc.CallOption) (*contract.SummariseMemoriesResponse, error) {
	out := &contract.SummariseMemoriesResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/events/"+pathSegment(in.GetEventId())+"/summarise", nil, in, out)
}

func (c *httpClient) Export(ctx context.Context, in *contract.ExportRequest, _ ...grpc.CallOption) (*contract.ExportResponse, error) {
	out := &contract.ExportResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/export", nil, in, out)
}

func (c *httpClient) Import(ctx context.Context, in *contract.ImportRequest, _ ...grpc.CallOption) (*contract.ImportResponse, error) {
	out := &contract.ImportResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/import", nil, in, out)
}

func (c *httpClient) ImportBatch(ctx context.Context, in *contract.ImportBatchRequest, _ ...grpc.CallOption) (*contract.ImportBatchResponse, error) {
	out := &contract.ImportBatchResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/import/batch", nil, in, out)
}

func (c *httpClient) Transfer(ctx context.Context, in *contract.TransferRequest, _ ...grpc.CallOption) (*contract.TransferResponse, error) {
	out := &contract.TransferResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/transfer", nil, in, out)
}

func (c *httpClient) Clear(ctx context.Context, in *contract.ClearRequest, _ ...grpc.CallOption) (*contract.ClearResponse, error) {
	out := &contract.ClearResponse{}

	return out, c.do(ctx, http.MethodPost, "/v1/clear", nil, in, out)
}

// doQuery renders req's set scalar fields as query parameters (dropping any bound to the URL path
// via exclude) and issues the request. It is the single funnel for the GET/DELETE routes, so the
// query-encoding error is handled in one place rather than in every method.
func (c *httpClient) doQuery(
	ctx context.Context,
	method string,
	path string,
	req proto.Message,
	exclude map[string]bool,
	out proto.Message,
) error {
	query, err := queryValues(req, exclude)
	if err != nil {
		return err
	}

	return c.do(ctx, method, path, query, nil, out)
}

// do performs one HTTP request against the gateway: it marshals body (when non-nil) as protojson,
// attaches the bearer token, and unmarshals a 2xx response into out. A non-2xx response is turned
// back into a gRPC status error so callers see the same codes.Code/message regardless of transport.
func (c *httpClient) do(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	body proto.Message,
	out proto.Message,
) error {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader

	if body != nil {
		data, err := protojson.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to encode request: %w", err)
		}

		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s failed: %w", target, err)
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return gatewayError(resp.StatusCode, data)
	}

	if out != nil {
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, out); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// gatewayError converts a non-2xx gateway response into a gRPC status error. The gateway renders an
// error as {"code": <grpc code>, "message": ...}; when the body does not parse that way the raw body
// (or a generic message) is surfaced under a code derived from the HTTP status.
func gatewayError(statusCode int, body []byte) error {
	var parsed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		return status.Error(codes.Code(parsed.Code), parsed.Message)
	}

	message := string(bytes.TrimSpace(body))
	if message == "" {
		message = http.StatusText(statusCode)
	}

	return status.Error(httpStatusToCode(statusCode), message)
}

// httpStatusToCode maps an HTTP status onto the closest gRPC code for the handful of statuses the
// gateway emits without a parseable code field.
func httpStatusToCode(statusCode int) codes.Code {
	switch statusCode {

	case http.StatusBadRequest:
		return codes.InvalidArgument

	case http.StatusUnauthorized:
		return codes.Unauthenticated

	case http.StatusForbidden:
		return codes.PermissionDenied

	case http.StatusNotFound:
		return codes.NotFound

	case http.StatusServiceUnavailable:
		return codes.Unavailable

	default:
		return codes.Unknown
	}
}

// queryValues renders a request message's set scalar fields as URL query parameters for the GET and
// DELETE gateway routes. protojson omits unset/zero fields, so only fields the caller actually set
// are sent; keys in exclude (those bound to the URL path) are dropped. The camelCase keys and enum
// value names protojson produces are exactly what the gateway's query parser accepts.
func queryValues(m proto.Message, exclude map[string]bool) (url.Values, error) {
	if m == nil {
		return nil, nil
	}

	data, err := protojson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("failed to encode query parameters: %w", err)
	}

	var raw map[string]json.RawMessage

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to encode query parameters: %w", err)
	}

	values := url.Values{}

	for k, rawValue := range raw {
		if exclude[k] {
			continue
		}

		var value any

		if err := json.Unmarshal(rawValue, &value); err != nil {
			continue
		}

		switch v := value.(type) {

		case string:
			values.Set(k, v)

		case bool:
			values.Set(k, strconv.FormatBool(v))

		case float64:
			values.Set(k, strconv.FormatFloat(v, 'f', -1, 64))

		case []any:
			// A repeated field becomes one query parameter per element, which is how the gateway
			// parses them - the default arm below would send the raw JSON array as a single value
			// and the gateway would reject it. Only reachable since a list request gained a
			// repeated field (the metadata filters); before that nothing exercised this.
			for _, element := range v {
				switch element := element.(type) {

				case string:
					values.Add(k, element)

				case bool:
					values.Add(k, strconv.FormatBool(element))

				case float64:
					values.Add(k, strconv.FormatFloat(element, 'f', -1, 64))

				default:
					encoded, err := json.Marshal(element)
					if err != nil {
						continue
					}

					values.Add(k, string(encoded))

				}
			}

		default:
			values.Set(k, string(rawValue))
		}
	}

	return values, nil
}

// pathSegment escapes a value for safe interpolation into a URL path.
func pathSegment(value string) string {
	return url.PathEscape(value)
}
