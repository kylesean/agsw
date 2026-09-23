package main

import "testing"

func TestVisualWidth(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"hello", 5},
		{"名字", 4},
		{"邮箱", 4},
		{"过期时间", 8},
		{"已过期", 6},
		{"refresh", 7},
		{"当前", 4},
		{"●", 1},
		{"A", 1},
		{"my-second-account", 17},
		{"57m0s 后", 8},
		{"是", 2},
		{"否", 2},
	}

	for _, tc := range tests {
		got := visualWidth(tc.input)
		if got != tc.want {
			t.Errorf("visualWidth(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestPadRight(t *testing.T) {
	tests := []struct {
		input string
		width int
		want  string
	}{
		{"名字", 8, "名字    "},
		{"A", 8, "A       "},
		{"邮箱", 6, "邮箱  "},
	}

	for _, tc := range tests {
		got := padRight(tc.input, tc.width)
		if got != tc.want {
			t.Errorf("padRight(%q, %d) = %q, want %q", tc.input, tc.width, got, tc.want)
		}
		if visualWidth(got) != tc.width {
			t.Errorf("visualWidth(padRight(%q, %d)) = %d, want %d", tc.input, tc.width, visualWidth(got), tc.width)
		}
	}
}
