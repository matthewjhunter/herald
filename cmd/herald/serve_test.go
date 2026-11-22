package main

import "testing"

func TestStaticClientFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		clientID string
		want     bool
		wantErr  bool
	}{
		{name: "unset keeps dynamic registration", value: "", clientID: "herald", want: false},
		{name: "true with a client id", value: "true", clientID: "herald", want: true},
		{name: "1 with a client id", value: "1", clientID: "herald", want: true},
		{name: "false", value: "false", clientID: "herald", want: false},
		{name: "false without a client id", value: "false", clientID: "", want: false},
		{name: "unparseable value is refused", value: "yes please", clientID: "herald", wantErr: true},
		{name: "true without a client id is refused", value: "true", clientID: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := staticClientFromEnv(tt.value, tt.clientID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
