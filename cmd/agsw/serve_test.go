package main

import (
	"context"
	"testing"
	"time"

	"github.com/kylesean/agsw/internal/pool"
)

func TestSelectorPickerReportsAccountSwitchOnce(t *testing.T) {
	now := time.Now()
	accounts := []*pool.Account{
		{Name: "A", Email: "a@example.com", AccessToken: "A", Expiry: now.Add(time.Hour)},
		{Name: "B", Email: "b@example.com", AccessToken: "B", Expiry: now.Add(time.Hour)},
	}
	sel := pool.NewSelector(accounts, nil)
	var switched []string
	picker := &selectorPicker{
		sel: sel,
		onSwitch: func(name, email string) {
			switched = append(switched, name+":"+email)
		},
	}

	if _, err := picker.Pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := picker.Pick(context.Background()); err != nil {
		t.Fatal(err)
	}
	sel.SetCooldown("A", now.Add(time.Hour))
	if _, err := picker.Pick(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{"A:a@example.com", "B:b@example.com"}
	if len(switched) != len(want) || switched[0] != want[0] || switched[1] != want[1] {
		t.Fatalf("switch events = %v, want %v", switched, want)
	}
}
