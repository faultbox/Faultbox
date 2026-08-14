package protocol

import "testing"

func TestCanonicalName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// camelCase / PascalCase boundaries.
		{"getOrder", "get_order"},
		{"GetOrder", "get_order"},
		{"GetOrderByID", "get_order_by_id"},
		{"listOrdersV2", "list_orders_v2"},
		{"HTTPServer", "http_server"},
		{"parseXMLDocument", "parse_xml_document"},
		// Already-snake input is idempotent.
		{"get_order", "get_order"},
		{"get_order_by_id", "get_order_by_id"},
		// Separator forms all collapse to the same name.
		{"get-order", "get_order"},
		{"get.order", "get_order"},
		{"get order", "get_order"},
		{"get//order", "get_order"},
		// Digits stay attached to the word they trail.
		{"order2Item", "order2_item"},
		{"v2", "v2"},
		// A leading digit would not be a valid attribute; prefix it.
		{"2faVerify", "op_2fa_verify"},
		// Single word.
		{"health", "health"},
		{"HEALTH", "health"},
	}
	for _, c := range cases {
		got, err := CanonicalName(c.in)
		if err != nil {
			t.Errorf("CanonicalName(%q) returned error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("CanonicalName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalName_NoAlphanumeric(t *testing.T) {
	for _, in := range []string{"", "///", "---", "  "} {
		if _, err := CanonicalName(in); err == nil {
			t.Errorf("CanonicalName(%q): expected error, got nil", in)
		}
	}
}

func TestContractInfo_String(t *testing.T) {
	cases := []struct {
		info ContractInfo
		want string
	}{
		{ContractInfo{Kind: ContractOpenAPI, Path: "orders.yaml", Version: "1.4.0"}, "openapi:orders.yaml@1.4.0"},
		{ContractInfo{Kind: ContractOpenAPI, Path: "orders.yaml"}, "openapi:orders.yaml"},
		{ContractInfo{Kind: ContractGRPC, Path: "orders.pb", Version: "orders.v1.OrderService"},
			"grpc:orders.pb#orders.v1.OrderService"},
		{ContractInfo{Kind: ContractGRPC, Path: "orders.pb"}, "grpc:orders.pb"},
	}
	for _, c := range cases {
		if got := c.info.String(); got != c.want {
			t.Errorf("ContractInfo%+v.String() = %q, want %q", c.info, got, c.want)
		}
	}
}

func TestOperationTable_ResolveSuggestsNearestName(t *testing.T) {
	table := newOperationTable(ContractInfo{Kind: ContractOpenAPI, Path: "x.yaml"})
	for _, name := range []string{"get_order", "list_orders", "create_order"} {
		if err := table.add(&Operation{Name: name, ContractName: name}); err != nil {
			t.Fatalf("add(%q): %v", name, err)
		}
	}
	table.finalize()

	if _, err := table.Resolve("api", "get_order"); err != nil {
		t.Fatalf("Resolve exact match failed: %v", err)
	}

	_, err := table.Resolve("api", "get_orders")
	if err == nil {
		t.Fatal("Resolve(\"get_orders\"): expected error, got nil")
	}
	if !contains(err.Error(), `did you mean "get_order"`) {
		t.Errorf("expected a get_order suggestion, got: %v", err)
	}
	if !contains(err.Error(), "3 operations available") {
		t.Errorf("expected the operation count in the error, got: %v", err)
	}

	// A name with nothing close to it should not invent a suggestion.
	_, err = table.Resolve("api", "zzzzzzzzzzzz")
	if err == nil {
		t.Fatal("expected error for unknown operation")
	}
	if contains(err.Error(), "did you mean") {
		t.Errorf("expected no suggestion for a distant name, got: %v", err)
	}
}

func TestOperationTable_AddRejectsCollision(t *testing.T) {
	table := newOperationTable(ContractInfo{Kind: ContractOpenAPI, Path: "x.yaml"})
	if err := table.add(&Operation{Name: "get_order", ContractName: "getOrder"}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	err := table.add(&Operation{Name: "get_order", ContractName: "get-order"})
	if err == nil {
		t.Fatal("expected a collision error")
	}
	for _, want := range []string{"getOrder", "get-order", "get_order", "rename="} {
		if !contains(err.Error(), want) {
			t.Errorf("collision error missing %q: %v", want, err)
		}
	}
}

func TestParamToString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"abc", "abc"},
		{int64(42), "42"},
		{int(7), "7"},
		{true, "true"},
		{false, "false"},
		{float64(42), "42"},   // integral floats lose the ".0"
		{float64(1.5), "1.5"}, // genuine fractions keep it
	}
	for _, c := range cases {
		got, err := paramToString(c.in)
		if err != nil {
			t.Errorf("paramToString(%v): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("paramToString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := paramToString([]any{1, 2}); err == nil {
		t.Error("paramToString([]any): expected error for an unsupported type")
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"get_order", "get_orders", 1},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
