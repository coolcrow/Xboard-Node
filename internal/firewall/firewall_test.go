package firewall

import "testing"

func TestDesiredRulesBothSplits(t *testing.T) {
	s := Spec{Managed: true, Allow: []AllowRule{{
		Source: "1.2.3.4",
		Ports:  []PortRule{{Port: 443, Proto: "both"}, {Port: 8443, Proto: "tcp"}},
	}}}
	got := desiredRules(s)
	want := map[string]bool{
		ruleText("1.2.3.4", 443, "tcp"):  true,
		ruleText("1.2.3.4", 443, "udp"):  true,
		ruleText("1.2.3.4", 8443, "tcp"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rules, want %d: %v", len(got), len(want), got)
	}
	for r := range want {
		if !got[r] {
			t.Errorf("missing %s", r)
		}
	}
}

func TestParseManagedShape(t *testing.T) {
	rules := []string{
		"rule family=ipv4 source address=43.139.120.168 port port=443 protocol=udp accept",
		"rule family=ipv4 source address=10.0.0.1 port port=22 protocol=tcp accept",
		"rule family=ipv4 source address=1.1.1.1 accept",
		"some other rich rule",
	}
	got := parseManaged(rules)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (port+protocol 形状): %v", len(got), got)
	}
}

func TestReconcileNoopWhenEqual(t *testing.T) {
	cur := parseManaged([]string{ruleText("5.6.7.8", 443, "tcp")})
	des := map[string]bool{ruleText("5.6.7.8", 443, "tcp"): true}
	add, rm := diff(des, cur)
	if len(add) != 0 || len(rm) != 0 {
		t.Errorf("expect noop, got add=%v rm=%v", add, rm)
	}
}
