package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/gravitational/teleport"
	api "github.com/gravitational/teleport/api/client"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/renew"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// TODO: CLI arguments for all of these
var (
	token      = "b344521794d75acada79ff780de66557"
	authServer = "localhost:3025"
	nodeName   = "test3"
	dest       = "dir:/Users/tim/certs"
)

func init() {
	// TODO: add a --debug
	utils.InitLogger(utils.LoggingForCLI, logrus.DebugLevel)
}

// TODO: need to store the bot's host ID and the name of the cluster
// we're connecting to - should we just dump that in the store?

func main() {
	if err := mainUserCerts(); err != nil {
		//if err := mainHostCerts(); err != nil {
		log.Fatalf("error: %s", trace.DebugReport(err))
	}
}

func insecureUserClient() (*auth.Client, error) {
	clock := clockwork.NewRealClock()
	tlsConfig := utils.TLSConfig([]uint16{})
	tlsConfig.Time = clock.Now

	tlsConfig.InsecureSkipVerify = true

	addr := utils.MustParseAddr(authServer)
	client, err := auth.NewClient(api.Config{
		Addrs: utils.NetAddrsToStrings([]utils.NetAddr{*addr}),
		Credentials: []api.Credentials{
			api.LoadTLS(tlsConfig),
		},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return client, nil
}

func mainUserCerts() error {
	// TODO: obviously the ssh public key needs to be persisted
	_, ssh, _, err := generateKeys()
	if err != nil {
		return trace.Wrap(err)
	}

	// TODO: borrow CA loading logic from auth.Register flow; this is totally
	// insecure
	client, err := insecureUserClient()
	if err != nil {
		return trace.WrapWithMessage(err, "Could not create an insecure auth client")
	}

	certs, err := client.GenerateInitialRenewableUserCerts(auth.RenewableCertsRequest{
		Token:     token,
		PublicKey: ssh,
	})
	if err != nil {
		return trace.WrapWithMessage(err, "Could not generate initial user certificates")
	}

	fmt.Printf("certs: %+v\n", certs)

	return nil
}

func generateUserCerts(tc *tls.Config) error {
	_, ssh, _, err := generateKeys()
	if err != nil {
		return trace.Wrap(err)
	}

	//addr := utils.MustParseAddr(authServer)

	// ds, err := renew.ParseDestinationSpec(dest)
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// store, err := renew.NewDestination(ds)
	// if err != nil {
	// 	log.Fatal(err)
	// }

	client, err := auth.NewClient(api.Config{
		Addrs:       []string{authServer},
		Credentials: []api.Credentials{api.LoadTLS(tc)},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	certs, err := client.GenerateInitialRenewableUserCerts(auth.RenewableCertsRequest{
		Token:     token,
		PublicKey: ssh,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	fmt.Printf("certs: %+v\n", certs)
	return nil
}

func mainHostCerts() error {
	addr := utils.MustParseAddr(authServer)

	ds, err := renew.ParseDestinationSpec(dest)
	if err != nil {
		return trace.Wrap(err)
	}

	store, err := renew.NewDestination(ds)
	if err != nil {
		return trace.Wrap(err)
	}

	id, err := renew.LoadIdentity(store)
	if err != nil {
		log.Println("could not load identity, starting new registration", err)
		privateKey, sshPublicKey, tlsPublicKey, err := generateKeys()
		if err != nil {
			return trace.Wrap(err)
		}
		hostID := uuid.New().String()
		id, err = auth.Register(auth.RegisterParams{
			Token: token,
			ID: auth.IdentityID{
				Role:     types.RoleNode,
				HostUUID: hostID,
				NodeName: nodeName,
			},
			Servers: []utils.NetAddr{*addr},
			CAPins:  []string{}, // TODO

			DNSNames:             nil,
			AdditionalPrincipals: nil,

			GetHostCredentials: client.HostCredentials,

			PrivateKey:   privateKey,
			PublicTLSKey: tlsPublicKey,
			PublicSSHKey: sshPublicKey,
		})
		if err != nil {
			return trace.WrapWithMessage(err, "could not register")
		}

		log.Println("registered with auth server, saving certs to disk!")

		if err := renew.SaveIdentity(id, store); err != nil {
			return trace.Wrap(err)
		}
	} else {
		// TODO: handle case where these certs are too old..
		log.Println("connecting to auth server with existing certificates")
	}

	tc, err := id.TLSConfig(nil)
	if err != nil {
		return trace.Wrap(err)
	}

	client, err := api.New(context.Background(), api.Config{
		Addrs:                    []string{authServer},
		Credentials:              []api.Credentials{api.LoadTLS(tc)},
		InsecureAddressDiscovery: true,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	if err := startServiceHeartbeat(client, id.ID.HostUUID); err != nil {
		return trace.Wrap(err)
	}

	log.Println("attempting to generate user certs")
	if err := generateUserCerts(tc); err != nil {
		return err
	}

	// log.Println("generating user certs")
	// userCerts, err := client.GenerateUserCerts(context.Background(), proto.UserCertsRequest{
	// 	PublicKey: id.KeySigner.PublicKey().Marshal(),
	// 	Username:  "test3",
	// 	Expires:   time.Now().UTC().Add(4 * time.Hour),
	// 	Usage:     proto.UserCertsRequest_All, // TODO: allow pinning to a specific node with NodeName
	// })
	// if err != nil {
	// 	log.Fatalln("could not generate user certs", err)
	// }

	//log.Println("generated user certs!")
	//log.Println("SSH:", string(userCerts.SSH))

	// log.Println("waiting for signals: ^C to rotate, ^\\ to exit")
	// ch := make(chan os.Signal, 1)
	// signal.Notify(ch, os.Interrupt)

	// for {
	// 	select {
	// 	case <-ch:
	// 		log.Println("rotating due to signal")
	// 	}
	// }

	return nil
}

func rotate(client auth.ClientI, hostID string) error {
	priv, ssh, tls, err := generateKeys()
	if err != nil {
		return err
	}

	id, err := auth.ReRegister(auth.ReRegisterParams{
		Client: client,
		ID: auth.IdentityID{
			Role:     types.RoleNode,
			HostUUID: hostID,
			NodeName: nodeName,
		},
		PrivateKey:           priv,
		PublicSSHKey:         ssh,
		PublicTLSKey:         tls,
		Rotation:             types.Rotation{}, // todo
		DNSNames:             nil,
		AdditionalPrincipals: nil,
	})
	if err != nil {
		return err
	}

	_ = id
	return nil
}

func startServiceHeartbeat(c *api.Client, hostID string) error {
	heartbeat, err := srv.NewHeartbeat(srv.HeartbeatConfig{
		Context:   context.Background(),
		Component: teleport.ComponentBot,
		Mode:      srv.HeartbeatModeBot,
		Announcer: announcerAdapter{c},
		GetServerInfo: func() (types.Resource, error) {
			bot := &types.BotV3{
				ResourceHeader: types.ResourceHeader{
					Metadata: types.Metadata{
						Name:      nodeName,
						Namespace: apidefaults.Namespace,
					},
					Version: types.V3,
					Kind:    types.KindBot,
				},
				Spec: types.BotSpecV3{
					HostID: hostID,
				},
			}
			bot.SetExpiry(time.Now().UTC().Add(apidefaults.ServerAnnounceTTL))
			return bot, nil
		},
		KeepAlivePeriod: apidefaults.ServerKeepAliveTTL,
		AnnouncePeriod:  apidefaults.ServerAnnounceTTL/2 + utils.RandomDuration(apidefaults.ServerAnnounceTTL/10),
		CheckPeriod:     defaults.HeartbeatCheckPeriod,
		ServerTTL:       apidefaults.ServerAnnounceTTL,
		OnHeartbeat: func(err error) {
			log.Println("heartbeat completed with error", err)
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}
	go func() {
		if err := heartbeat.Run(); err != nil {
			log.Println("heartbeat ended with error")
		}
	}()
	return nil
}

func generateKeys() (private, sshpub, tlspub []byte, err error) {
	privateKey, publicKey, err := native.GenerateKeyPair("")
	if err != nil {
		return nil, nil, nil, err
	}

	sshPrivateKey, err := ssh.ParseRawPrivateKey(privateKey)
	if err != nil {
		return nil, nil, nil, err
	}

	tlsPublicKey, err := tlsca.MarshalPublicKeyFromPrivateKeyPEM(sshPrivateKey)
	if err != nil {
		return nil, nil, nil, err
	}

	return privateKey, publicKey, tlsPublicKey, nil
}

// API client can't upsert core components like auth servers and proxies,
// so just nop those calls

type announcerAdapter struct{ *api.Client }

func (a announcerAdapter) UpsertAuthServer(s types.Server) error { return nil }
func (a announcerAdapter) UpsertProxy(s types.Server) error      { return nil }

// API client doesn't implement all of ClientI

type clientiAdapter struct{ *api.Client }