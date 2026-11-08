//go:build linux

package agent

import (
	"context"
	"testing"
)

func aliasProfile() PreparationProfile {
	p := preparationFixtureProfile()
	p.Network, p.NetworkAlias = "stack_net", "tp-plugin-homeassistant"
	return p
}

func TestNetworkAliasProfileValidation(t *testing.T) {
	if !validPreparationProfile(aliasProfile()) {
		t.Fatal("valid alias rejected")
	}
	for _, alias := range []string{"-bad", "bad-", "Upper", "has space", "a.b", "x_y"} {
		p := aliasProfile()
		p.NetworkAlias = alias
		if validPreparationProfile(p) {
			t.Fatalf("alias %q accepted", alias)
		}
	}
	p := preparationFixtureProfile() // alias without a network is meaningless
	p.NetworkAlias = "tp-plugin-x"
	if validPreparationProfile(p) {
		t.Fatal("alias without network accepted")
	}
}

func TestNetworkAliasIsStableAcrossGenerationsAndChecked(t *testing.T) {
	profile := aliasProfile()
	one := preparationCreate(containerPreparation{Instance: "i", Generation: 1, Profile: profile}, "h")
	two := preparationCreate(containerPreparation{Instance: "i", Generation: 2, Profile: profile}, "h")
	for _, request := range []dockerCreateRequest{one, two} {
		got := request.NetworkingConfig.EndpointsConfig["stack_net"].Aliases
		if len(got) != 1 || got[0] != "tp-plugin-homeassistant" {
			t.Fatalf("alias not set: %#v", got)
		}
	}
	if preparationCreate(containerPreparation{Profile: preparationFixtureProfile()}, "h").NetworkingConfig != nil {
		t.Fatal("no alias must not add a networking config")
	}
}

func TestPreparedContainerWithoutAliasIsNotTrusted(t *testing.T) {
	for _, test := range []struct {
		name    string
		aliases []string
		trusted bool
	}{
		{"with-alias", []string{"tp-plugin-homeassistant", "abc"}, true},
		{"without-alias", []string{"abc"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := initialized(t)
			engine := newPreparationFixtureEngine()
			profile := aliasProfile()
			preparer, err := newDockerPreparer(store, engine, []PreparationProfile{profile})
			if err != nil {
				t.Fatal(err)
			}
			request := PrepareContainerRequest{Instance: "instance-a", Generation: 1, RequestKey: "k", Profile: profile.Name}
			if err := store.AcceptGeneration(request.Instance, 1); err != nil {
				t.Fatal(err)
			}
			original, err := preparer.Prepare(context.Background(), request)
			if err != nil || original.State != PreparationPrepared {
				t.Fatal(err)
			}
			c := engine.containers[original.ContainerName]
			c.NetworkSettings.Networks = map[string]struct{ Aliases []string }{"stack_net": {Aliases: test.aliases}}
			engine.containers[original.ContainerName] = c
			observation, err := preparer.Query(context.Background(), request.Instance, 1, request.RequestKey)
			if err != nil {
				t.Fatal(err)
			}
			if got := observation.State == PreparationPrepared; got != test.trusted {
				t.Fatalf("trusted=%v want %v: %#v", got, test.trusted, observation)
			}
		})
	}
}
