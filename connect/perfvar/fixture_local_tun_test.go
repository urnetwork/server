// The real correctness constructor must admit its allocated carrier addresses
// before the provider's acknowledged registration can make forward progress.
package perfvar

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Portable fixtures intentionally have no geography databases. Bind the
// actual Config selection before exercising the same model entry as announce;
// a missing allocator override then fails here, not at the 90s provide guard.
func TestPerfvarCorrectnessFixtureAdmitsAllocatedTunAddresses(t *testing.T) {
	portable := os.Getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES") == "1"
	if portable {
		root := os.Getenv("WARP_TEST_ENV_PORTABLE_ROOT")
		if root == "" {
			t.Fatal("portable local tun witness has no owned fixture root")
		}
		configRoot, err := filepath.EvalSymlinks(filepath.Join(root, "config"))
		if err != nil {
			t.Fatal(err)
		}
		selected, err := server.Config.ResourcePath("settings.yml")
		if err != nil {
			t.Fatal(err)
		}
		selected, err = filepath.EvalSymlinks(selected)
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(configRoot, selected)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			t.Fatal("local tun settings resolved outside the owned generated fixture")
		}
		for _, name := range []string{"mmdb/geolite2.mmdb", "arindb/arin.mmdb"} {
			if _, err := os.Lstat(filepath.Join(configRoot, name)); !os.IsNotExist(err) {
				t.Fatalf("portable local tun witness unexpectedly has database %s: %v", name, err)
			}
			if selected, err := server.Config.ResourcePath(name); err == nil {
				t.Fatalf("portable local tun witness selected an ambient database %s: %s", name, selected)
			}
		}
	}

	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profile := initialNetworkProfiles(20260811)["clean-lan"]
		admitted := 0
		registrationAcknowledged := false
		hooks := &fullTunConstructionTestHooks{
			afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
				if stage == fullTunConstructionStageProviderRegistration {
					if admitted != 3 {
						return fmt.Errorf("provider acknowledgement preceded allocated-address admission: %d", admitted)
					}
					registrationAcknowledged = true
					return nil
				}
				if stage != fullTunConstructionStageProviderCarrierTun {
					return nil
				}
				addresses := path.providerCarrierTun.LocalAddresses()
				if len(addresses) != 2 || !addresses[0].Is4() || !addresses[1].Is6() {
					return fmt.Errorf("actual correctness carrier lost dual-stack allocation: %v", addresses)
				}
				for _, address := range addresses {
					rawAddresses := []netip.Addr{address}
					if address.Is4() {
						rawAddresses = append(rawAddresses, netip.AddrFrom16(address.As16()))
					}
					for _, rawAddress := range rawAddresses {
						err := func() (returnErr error) {
							defer func() {
								if failure := recover(); failure != nil {
									returnErr = fmt.Errorf("local tun model admission raised before provide acknowledgement: %v", failure)
								}
							}()
							request := &http.Request{RemoteAddr: netip.AddrPortFrom(rawAddress, 1).String()}
							resolved, err := session.ResolveClientAddressFromRequest(request)
							if err != nil {
								return err
							}
							resolvedAddress, err := netip.ParseAddrPort(resolved)
							if err != nil || resolvedAddress.Addr() != address {
								return fmt.Errorf("actual ingress did not normalize allocated address: %s %v", resolved, err)
							}
							clientId := server.NewId()
							model.Testing_CreateDevice(path.ctx, path.environment.networkId, server.NewId(), clientId, "local tun fixture", "local tun fixture")
							handlerId := model.CreateNetworkClientHandler(path.ctx)
							family := 4
							if address.Is6() {
								family = 6
							}
							connectionId, _, err := controller.ConnectNetworkClientWithIpFamily(
								path.ctx, clientId, resolved, handlerId, 0, family, path.environment.networkId,
							)
							if err != nil {
								return fmt.Errorf("actual local tun controller admission: %w", err)
							}
							defer func() {
								ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
								defer cancel()
								if err := model.DisconnectNetworkClient(ctx, connectionId); err != nil {
									returnErr = fmt.Errorf("disconnect allocated-address witness: %v (admission: %v)", err, returnErr)
								}
							}()
							if err := model.GetNetworkClientConnectionStatus(path.ctx, connectionId).Err(); err != nil {
								return fmt.Errorf("local tun model did not commit a live client connection: %w", err)
							}
							if portable {
								location, err := server.GetIpInfoFromString(resolvedAddress.Addr().String())
								if err != nil || location == nil || location.CountryCode != "zz" || location.Country != "Fixture Country" || location.Region != "Fixture Region" || location.City != "Fixture City" {
									return fmt.Errorf("local tun model left the synthetic location fixture: %v %+v", err, location)
								}
							}
							return nil
						}()
						if err != nil {
							return err
						}
						admitted++
					}
				}
				return nil
			},
		}
		fixture, err := newPerfvarCorrectnessFixtureWithHooks(
			t, fullTunRouteExchangeH1, profile, profile, profile,
			defaultTunResourceProfile(), 5*time.Minute, hooks,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer fixture.close()
		if admitted != 3 || !registrationAcknowledged {
			t.Fatalf("allocated-address admission did not reach real provide acknowledgement: admissions=%d acknowledged=%t", admitted, registrationAcknowledged)
		}
		t.Log("actual-local-tun-model-admission-and-provide-ack-complete")
	})
}
