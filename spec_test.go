package camfarm

import "testing"

func TestAuthSpecDefaultIsOpen(t *testing.T) {
	s := Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a"}}}
	valid, err := s.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if valid.Cameras[0].Auth != (AuthSpec{}) {
		t.Errorf("Auth = %+v, want zero value", valid.Cameras[0].Auth)
	}
}

func TestAuthSpecPartialCredentialsRefused(t *testing.T) {
	s := Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a", Auth: AuthSpec{Username: "admin"}}}}
	if _, err := s.validate(); err == nil {
		t.Fatal("validate: want error for username set without password")
	}
}
