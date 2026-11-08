package config

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestUsageSourceSelectionFencesDuplicateAndProtectedABABeforeNotification(t *testing.T) {
	for _, name := range []string{"duplicate-removal", "official-reference", "official-epoch"} {
		t.Run(name, func(t *testing.T) {
			store := usageRevisionStore(t)
			if err := store.Update(func(c *Config) error {
				duplicate := c.LocalAccounts[0]
				duplicate.ID = "duplicate"
				c.LocalAccounts = append(c.LocalAccounts, duplicate, LocalAccount{ID: "official", SourceType: "official_account", CredentialRef: "protected-A", CredentialEpoch: 1})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			initial := store.Get()
			notified, release := make(chan Config, 2), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			store.OnChange(func(old, next Config) { notified <- next; <-release })
			updates := make(chan error, 2)
			previous := initial.UsageSourceRevision
			for step := range 2 {
				go func(step int) {
					updates <- store.Update(func(c *Config) error {
						if step == 1 {
							c.LocalAccounts = initial.Clone().LocalAccounts
							return nil
						}
						switch name {
						case "duplicate-removal":
							c.LocalAccounts = append(c.LocalAccounts[:1], c.LocalAccounts[2:]...)
						case "official-reference":
							c.LocalAccounts[2].CredentialRef = "protected-B"
						case "official-epoch":
							c.LocalAccounts[2].CredentialEpoch = 2
						}
						return nil
					})
				}(step)
				select {
				case published := <-notified:
					current := store.Get()
					if current.UsageSourceRevision == previous || current.UsageSourceRevision == initial.UsageSourceRevision || current.UsageSourceRevision != published.UsageSourceRevision {
						t.Fatal("selected source mutation did not publish its own revision before callbacks")
					}
					if !reflect.DeepEqual(initial.UsageSourceTargets(), current.UsageSourceTargets()) {
						t.Fatal("selection fencing changed the legacy deduplicated provider targets")
					}
					previous = current.UsageSourceRevision
				case <-time.After(3 * time.Second):
					t.Fatal("fixture notification did not reach barrier")
				}
			}
			if initial.UsageSourceFingerprint() != store.Get().UsageSourceFingerprint() {
				t.Fatal("fixture failed to restore source content")
			}
			once.Do(func() { close(release) })
			for range 2 {
				if err := <-updates; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
