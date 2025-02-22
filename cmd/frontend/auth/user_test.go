package auth

import (
	"context"
	"fmt"
	"testing"

	mockrequire "github.com/derision-test/go-mockgen/testutil/require"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/auth"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/database/dbmocks"
	"github.com/sourcegraph/sourcegraph/internal/errcode"
	"github.com/sourcegraph/sourcegraph/internal/extsvc"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

// TestGetAndSaveUser ensures the correctness of the GetAndSaveUser function.
//
// 🚨 SECURITY: This guarantees the integrity of the identity resolution process (ensuring that new
// external accounts are linked to the appropriate user account)
func TestGetAndSaveUser(t *testing.T) {
	type innerCase struct {
		description string
		actorUID    int32
		op          GetAndSaveUserOp

		// if true, then will expect same output if op.CreateIfNotExist is true or false
		createIfNotExistIrrelevant bool

		// expected return values
		expUserID         int32
		expSafeErr        string
		expErr            error
		expNewUserCreated bool

		// expected side effects
		expSavedExtAccts                 map[int32][]extsvc.AccountSpec
		expUpdatedUsers                  map[int32][]database.UserUpdate
		expCreatedUsers                  map[int32]database.NewUser
		expCalledGrantPendingPermissions bool
		expCalledCreateUserSyncJob       bool
	}
	type outerCase struct {
		description string
		mock        mockParams
		innerCases  []innerCase
	}

	unexpectedErr := errors.New("unexpected err")

	oneUser := []userInfo{{
		user: types.User{ID: 1, Username: "u1"},
		extAccts: []extsvc.AccountSpec{
			ext("st1", "s1", "c1", "s1/u1"),
		},
		emails: []string{"u1@example.com"},
	}}
	getOneUserOp := GetAndSaveUserOp{
		ExternalAccount: ext("st1", "s1", "c1", "s1/u1"),
		UserProps:       userProps("u1", "u1@example.com"),
	}
	getNonExistentUserCreateIfNotExistOp := GetAndSaveUserOp{
		ExternalAccount:  ext("st1", "s1", "c1", "nonexistent"),
		UserProps:        userProps("nonexistent", "nonexistent@example.com"),
		CreateIfNotExist: true,
	}

	mainCase := outerCase{
		description: "no unexpected errors",
		mock: mockParams{
			userInfos: []userInfo{
				{
					user: types.User{ID: 1, Username: "u1"},
					extAccts: []extsvc.AccountSpec{
						ext("st1", "s1", "c1", "s1/u1"),
					},
					emails: []string{"u1@example.com"},
				},
				{
					user: types.User{ID: 2, Username: "u2"},
					extAccts: []extsvc.AccountSpec{
						ext("st1", "s1", "c1", "s1/u2"),
					},
					emails: []string{"u2@example.com"},
				},
				{
					user:     types.User{ID: 3, Username: "u3"},
					extAccts: []extsvc.AccountSpec{},
					emails:   []string{},
				},
			},
		},
		// TODO(beyang): add non-verified email cases
		innerCases: []innerCase{
			{
				description: "ext acct exists, user has same username and email",
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "s1/u1"),
					UserProps:       userProps("u1", "u1@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s1", "c1", "s1/u1")},
				},
				expNewUserCreated: false,
			},
			{
				description: "ext acct exists, username and email don't exist",
				// Note: for now, we drop the non-matching email; in the future, we may want to
				// save this as a new verified user email
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "s1/u1"),
					UserProps:       userProps("doesnotexist", "doesnotexist@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s1", "c1", "s1/u1")},
				},
				expNewUserCreated: false,
			},
			{
				description: "ext acct exists, email belongs to another user",
				// In this case, the external account is already mapped, so we ignore the email
				// inconsistency
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "s1/u1"),
					UserProps:       userProps("u1", "u2@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s1", "c1", "s1/u1")},
				},
				expNewUserCreated: false,
			},
			{
				description: "ext acct doesn't exist, user with username and email exists",
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s-new", "c1", "s-new/u1"),
					UserProps:       userProps("u1", "u1@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s-new", "c1", "s-new/u1")},
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                false,
			},
			{
				description: "ext acct doesn't exist, user with username exists but email doesn't exist",
				// Note: if the email doesn't match, the user effectively doesn't exist from our POV
				op: GetAndSaveUserOp{
					ExternalAccount:  ext("st1", "s-new", "c1", "s-new/u1"),
					UserProps:        userProps("u1", "doesnotmatch@example.com"),
					CreateIfNotExist: true,
				},
				expSafeErr: "Username \"u1\" already exists, but no verified email matched \"doesnotmatch@example.com\"",
				expErr:     database.MockCannotCreateUserUsernameExistsErr,
			},
			{
				description: "ext acct doesn't exist, user with email exists but username doesn't exist",
				// We treat this as a resolved user and ignore the non-matching username
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s-new", "c1", "s-new/u1"),
					UserProps:       userProps("doesnotmatch", "u1@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s-new", "c1", "s-new/u1")},
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                false,
			},
			{
				description: "ext acct doesn't exist, username and email don't exist, should create user",
				op: GetAndSaveUserOp{
					ExternalAccount:  ext("st1", "s1", "c1", "s1/u-new"),
					UserProps:        userProps("u-new", "u-new@example.com"),
					CreateIfNotExist: true,
				},
				expUserID: 10001,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					10001: {ext("st1", "s1", "c1", "s1/u-new")},
				},
				expCreatedUsers: map[int32]database.NewUser{
					10001: userProps("u-new", "u-new@example.com"),
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                true,
			},
			{
				description: "ext acct doesn't exist, username and email don't exist, should NOT create user",
				op: GetAndSaveUserOp{
					ExternalAccount:  ext("st1", "s1", "c1", "s1/u-new"),
					UserProps:        userProps("u-new", "u-new@example.com"),
					CreateIfNotExist: false,
				},
				expSafeErr:        "User account with verified email \"u-new@example.com\" does not exist. Ask a site admin to create your account and then verify your email.",
				expErr:            database.MockUserNotFoundErr,
				expNewUserCreated: false,
			},
			{
				description: "ext acct exists, (ignore username and email), authenticated",
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "s1/u2"),
					UserProps:       userProps("ignore", "ignore"),
				},
				createIfNotExistIrrelevant: true,
				actorUID:                   2,
				expUserID:                  2,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					2: {ext("st1", "s1", "c1", "s1/u2")},
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                false,
			},
			{
				description: "ext acct doesn't exist, email and username match, authenticated",
				actorUID:    1,
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "s1/u1"),
					UserProps:       userProps("u1", "u1@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s1", "c1", "s1/u1")},
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                false,
			},
			{
				description: "ext acct doesn't exist, email matches but username doesn't, authenticated",
				// The non-matching username is ignored
				actorUID: 1,
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "s1/u1"),
					UserProps:       userProps("doesnotmatch", "u1@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s1", "c1", "s1/u1")},
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                false,
			},
			{
				description: "ext acct doesn't exist, email doesn't match existing user, authenticated",
				// The non-matching email is ignored. In the future, we may want to save this as
				// a verified user email, but this would be more complicated, because the email
				// might be associated with an existing user (in which case the authentication
				// should fail).
				actorUID: 1,
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s-new", "c1", "s-new/u1"),
					UserProps:       userProps("u1", "doesnotmatch@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s-new", "c1", "s-new/u1")},
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                false,
			},
			{
				description: "ext acct doesn't exist, user has same username, lookupByUsername=true",
				op: GetAndSaveUserOp{
					ExternalAccount:  ext("st1", "s1", "c1", "doesnotexist"),
					UserProps:        userProps("u1", ""),
					LookUpByUsername: true,
				},
				createIfNotExistIrrelevant: true,
				expUserID:                  1,
				expSavedExtAccts: map[int32][]extsvc.AccountSpec{
					1: {ext("st1", "s1", "c1", "doesnotexist")},
				},
				expCalledGrantPendingPermissions: true,
				expCalledCreateUserSyncJob:       true,
				expNewUserCreated:                false,
			},
		},
	}
	errorCases := []outerCase{
		{
			description: "lookupUserAndSaveErr",
			mock:        mockParams{lookupUserAndSaveErr: unexpectedErr, userInfos: oneUser},
			innerCases: []innerCase{{
				op:                         getOneUserOp,
				createIfNotExistIrrelevant: true,
				expSafeErr:                 "Unexpected error looking up the Sourcegraph user account associated with the external account. Ask a site admin for help.",
				expErr:                     unexpectedErr,
			}},
		},
		{
			description: "createUserAndSaveErr",
			mock:        mockParams{createUserAndSaveErr: unexpectedErr, userInfos: oneUser},
			innerCases: []innerCase{{
				op:         getNonExistentUserCreateIfNotExistOp,
				expSafeErr: "Unable to create a new user account due to a unexpected error. Ask a site admin for help.",
				expErr:     errors.Wrapf(unexpectedErr, `username: "nonexistent", email: "nonexistent@example.com"`),
			}},
		},
		{
			description: "associateUserAndSaveErr",
			mock:        mockParams{associateUserAndSaveErr: unexpectedErr, userInfos: oneUser},
			innerCases: []innerCase{{
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "nonexistent"),
					UserProps:       userProps("u1", "u1@example.com"),
				},
				expSafeErr: "Unexpected error associating the external account with your Sourcegraph user. The most likely cause for this problem is that another Sourcegraph user is already linked with this external account. A site admin or the other user can unlink the account to fix this problem.",
				expErr:     unexpectedErr,
			}},
		},
		{
			description: "getByVerifiedEmailErr",
			mock:        mockParams{getByVerifiedEmailErr: unexpectedErr, userInfos: oneUser},
			innerCases: []innerCase{{
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "nonexistent"),
					UserProps:       userProps("u1", "u1@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expSafeErr:                 "Unexpected error looking up the Sourcegraph user by verified email. Ask a site admin for help.",
				expErr:                     unexpectedErr,
			}},
		},
		{
			description: "getByIDErr",
			mock:        mockParams{getByIDErr: unexpectedErr, userInfos: oneUser},
			innerCases: []innerCase{{
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "nonexistent"),
					UserProps:       userProps("u1", "u1@example.com"),
				},
				createIfNotExistIrrelevant: true,
				expSafeErr:                 "Unexpected error getting the Sourcegraph user account. Ask a site admin for help.",
				expErr:                     unexpectedErr,
			}},
		},
		{
			description: "updateErr",
			mock:        mockParams{updateErr: unexpectedErr, userInfos: oneUser},
			innerCases: []innerCase{{
				op: GetAndSaveUserOp{
					ExternalAccount: ext("st1", "s1", "c1", "nonexistent"),
					UserProps: database.NewUser{
						Email:           "u1@example.com",
						EmailIsVerified: true,
						Username:        "u1",
						DisplayName:     "New Name",
					},
				},
				createIfNotExistIrrelevant: true,
				expSafeErr:                 "Unexpected error updating the Sourcegraph user account with new user profile information from the external account. Ask a site admin for help.",
				expErr:                     unexpectedErr,
			}},
		},
	}

	allCases := append(append([]outerCase{}, mainCase), errorCases...)
	for _, oc := range allCases {
		t.Run(oc.description, func(t *testing.T) {
			for _, c := range oc.innerCases {
				if c.expSavedExtAccts == nil {
					c.expSavedExtAccts = map[int32][]extsvc.AccountSpec{}
				}
				if c.expUpdatedUsers == nil {
					c.expUpdatedUsers = map[int32][]database.UserUpdate{}
				}
				if c.expCreatedUsers == nil {
					c.expCreatedUsers = map[int32]database.NewUser{}
				}

				createIfNotExistVals := []bool{c.op.CreateIfNotExist}
				if c.createIfNotExistIrrelevant {
					createIfNotExistVals = []bool{false, true}
				}
				for _, createIfNotExist := range createIfNotExistVals {
					description := c.description
					if len(createIfNotExistVals) == 2 {
						description = fmt.Sprintf("%s, createIfNotExist=%v", description, createIfNotExist)
					}
					t.Run("", func(t *testing.T) {
						t.Logf("Description: %q", description)
						m := newMocks(t, oc.mock)

						ctx := context.Background()
						if c.actorUID != 0 {
							ctx = actor.WithActor(context.Background(), &actor.Actor{UID: c.actorUID})
						}
						op := c.op
						op.CreateIfNotExist = createIfNotExist
						newUserCreated, userID, safeErr, err := GetAndSaveUser(ctx, m.DB(), op)

						if userID != c.expUserID {
							t.Errorf("mismatched userID, want: %v, but got %v", c.expUserID, userID)
						}

						if diff := cmp.Diff(safeErr, c.expSafeErr); diff != "" {
							t.Errorf("mismatched safeErr, got != want, diff(-got, +want):\n%s", diff)
						}

						if !errors.Is(err, c.expErr) {
							t.Errorf("mismatched errors, want %#v, but got %#v", c.expErr, err)
						}

						if diff := cmp.Diff(m.savedExtAccts, c.expSavedExtAccts); diff != "" {
							t.Errorf("mismatched side-effect savedExtAccts, got != want, diff(-got, +want):\n%s", diff)
						}

						if diff := cmp.Diff(m.updatedUsers, c.expUpdatedUsers); diff != "" {
							t.Errorf("mismatched side-effect updatedUsers, got != want, diff(-got, +want):\n%s", diff)
						}

						if diff := cmp.Diff(m.createdUsers, c.expCreatedUsers); diff != "" {
							t.Errorf("mismatched side-effect createdUsers, got != want, diff(-got, +want):\n%s", diff)
						}

						if c.expCalledCreateUserSyncJob != m.calledCreateUserSyncJob {
							t.Fatalf("calledCreateUserSyncJob: want %v but got %v", c.expCalledGrantPendingPermissions, m.calledCreateUserSyncJob)
						}

						if c.expCalledGrantPendingPermissions != m.calledGrantPendingPermissions {
							t.Fatalf("calledGrantPendingPermissions: want %v but got %v", c.expCalledGrantPendingPermissions, m.calledGrantPendingPermissions)
						}

						if newUserCreated != c.expNewUserCreated {
							t.Errorf("mismatched newUserCreated, want %v but got %v", c.expNewUserCreated, newUserCreated)
						}
					})
				}
			}
		})
	}

	t.Run("Sourcegraph operator actor should be propagated", func(t *testing.T) {
		ctx := context.Background()

		errNotFound := &errcode.Mock{
			IsNotFound: true,
		}
		gss := dbmocks.NewMockGlobalStateStore()
		gss.GetFunc.SetDefaultReturn(database.GlobalState{SiteID: "a"}, nil)
		usersStore := dbmocks.NewMockUserStore()
		usersStore.GetByVerifiedEmailFunc.SetDefaultReturn(nil, errNotFound)
		externalAccountsStore := dbmocks.NewMockUserExternalAccountsStore()
		externalAccountsStore.LookupUserAndSaveFunc.SetDefaultReturn(0, errNotFound)
		externalAccountsStore.CreateUserAndSaveFunc.SetDefaultHook(func(ctx context.Context, _ database.NewUser, _ extsvc.AccountSpec, _ extsvc.AccountData) (*types.User, error) {
			require.True(t, actor.FromContext(ctx).SourcegraphOperator, "the actor should be a Sourcegraph operator")
			return &types.User{ID: 1}, nil
		})
		eventLogsStore := dbmocks.NewMockEventLogStore()
		eventLogsStore.BulkInsertFunc.SetDefaultHook(func(ctx context.Context, _ []*database.Event) error {
			require.True(t, actor.FromContext(ctx).SourcegraphOperator, "the actor should be a Sourcegraph operator")
			return nil
		})
		permsSyncJobsStore := dbmocks.NewMockPermissionSyncJobStore()
		db := dbmocks.NewMockDB()
		db.GlobalStateFunc.SetDefaultReturn(gss)
		db.UsersFunc.SetDefaultReturn(usersStore)
		db.UserExternalAccountsFunc.SetDefaultReturn(externalAccountsStore)
		db.AuthzFunc.SetDefaultReturn(dbmocks.NewMockAuthzStore())
		db.EventLogsFunc.SetDefaultReturn(eventLogsStore)
		db.PermissionSyncJobsFunc.SetDefaultReturn(permsSyncJobsStore)

		_, _, _, err := GetAndSaveUser(
			ctx,
			db,
			GetAndSaveUserOp{
				UserProps: database.NewUser{
					EmailIsVerified: true,
				},
				ExternalAccount: extsvc.AccountSpec{
					ServiceType: auth.SourcegraphOperatorProviderType,
				},
				ExternalAccountData: extsvc.AccountData{},
				CreateIfNotExist:    true,
			},
		)
		require.NoError(t, err)
		mockrequire.Called(t, externalAccountsStore.CreateUserAndSaveFunc)
	})
}

type userInfo struct {
	user     types.User
	extAccts []extsvc.AccountSpec
	emails   []string
}

func newMocks(t *testing.T, m mockParams) *mocks {
	// validation
	extAcctIDs := make(map[string]struct{})
	userIDs := make(map[int32]struct{})
	usernames := make(map[string]struct{})
	emails := make(map[string]struct{})
	for _, u := range m.userInfos {
		if _, exists := usernames[u.user.Username]; exists {
			t.Fatal("mocks: dup username")
		}
		usernames[u.user.Username] = struct{}{}

		if _, exists := userIDs[u.user.ID]; exists {
			t.Fatal("mocks: dup user ID")
		}
		userIDs[u.user.ID] = struct{}{}

		for _, email := range u.emails {
			if _, exists := emails[email]; exists {
				t.Fatal("mocks: dup email")
			}
			emails[email] = struct{}{}
		}
		for _, extAcct := range u.extAccts {
			if _, exists := extAcctIDs[extAcct.AccountID]; exists {
				t.Fatal("mocks: dup ext account ID")
			}
			extAcctIDs[extAcct.AccountID] = struct{}{}
		}
	}

	return &mocks{
		mockParams:    m,
		t:             t,
		savedExtAccts: make(map[int32][]extsvc.AccountSpec),
		updatedUsers:  make(map[int32][]database.UserUpdate),
		createdUsers:  make(map[int32]database.NewUser),
		nextUserID:    10001,
	}
}

func TestMetadataOnlyAutomaticallySetOnFirstOccurrence(t *testing.T) {
	t.Parallel()

	gss := dbmocks.NewMockGlobalStateStore()
	gss.GetFunc.SetDefaultReturn(database.GlobalState{SiteID: "a"}, nil)

	user := &types.User{ID: 1, DisplayName: "", AvatarURL: ""}

	users := dbmocks.NewMockUserStore()
	users.GetByIDFunc.SetDefaultReturn(user, nil)
	users.UpdateFunc.SetDefaultHook(func(_ context.Context, userID int32, update database.UserUpdate) error {
		user.DisplayName = *update.DisplayName
		user.AvatarURL = *update.AvatarURL
		return nil
	})

	externalAccounts := dbmocks.NewMockUserExternalAccountsStore()
	externalAccounts.LookupUserAndSaveFunc.SetDefaultReturn(user.ID, nil)

	db := dbmocks.NewMockDB()
	db.GlobalStateFunc.SetDefaultReturn(gss)
	db.UsersFunc.SetDefaultReturn(users)
	db.UserExternalAccountsFunc.SetDefaultReturn(externalAccounts)

	// Customers can always set their own display name and avatar URL values, but when
	// we encounter them via e.g. code host logins, we don't want to override anything
	// currently present. This puts the customer in full control of the experience.
	tests := []struct {
		description     string
		displayName     string
		wantDisplayName string
		avatarURL       string
		wantAvatarURL   string
	}{
		{
			description:     "setting initial value",
			displayName:     "first",
			wantDisplayName: "first",
			avatarURL:       "first.jpg",
			wantAvatarURL:   "first.jpg",
		},
		{
			description:     "applying an update",
			displayName:     "second",
			wantDisplayName: "first",
			avatarURL:       "second.jpg",
			wantAvatarURL:   "first.jpg",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			ctx := context.Background()
			op := GetAndSaveUserOp{
				ExternalAccount: ext("github", "fake-service", "fake-client", "account-u1"),
				UserProps:       database.NewUser{DisplayName: test.displayName, AvatarURL: test.avatarURL},
			}
			if _, _, _, err := GetAndSaveUser(ctx, db, op); err != nil {
				t.Fatal(err)
			}
			if user.DisplayName != test.wantDisplayName {
				t.Errorf("DisplayName: got %q, want %q", user.DisplayName, test.wantDisplayName)
			}
			if user.AvatarURL != test.wantAvatarURL {
				t.Errorf("AvatarURL: got %q, want %q", user.DisplayName, test.wantAvatarURL)
			}
		})
	}
}

type mockParams struct {
	userInfos               []userInfo
	lookupUserAndSaveErr    error
	createUserAndSaveErr    error
	associateUserAndSaveErr error
	getByVerifiedEmailErr   error
	getByUsernameErr        error //nolint:structcheck
	getByIDErr              error
	updateErr               error
}

// mocks provide mocking. It should only be used for one call of auth.GetAndSaveUser, because saves
// are recorded in the mock struct but will not be reflected in the return values of the mocked
// methods.
type mocks struct {
	mockParams
	t *testing.T

	// savedExtAccts tracks all ext acct "saves" for a given user ID
	savedExtAccts map[int32][]extsvc.AccountSpec

	// createdUsers tracks user creations by user ID
	createdUsers map[int32]database.NewUser

	// updatedUsers tracks all user updates for a given user ID
	updatedUsers map[int32][]database.UserUpdate

	// nextUserID is the user ID of the next created user.
	nextUserID int32

	// calledGrantPendingPermissions tracks if database.Authz.GrantPendingPermissions method is called.
	calledGrantPendingPermissions bool

	// calledCreateUserSyncJob tracks if database.PermissionsSyncJobs.CreateUserSyncJob method is called.
	calledCreateUserSyncJob bool
}

func (m *mocks) DB() database.DB {
	gss := dbmocks.NewMockGlobalStateStore()
	gss.GetFunc.SetDefaultReturn(database.GlobalState{SiteID: "a"}, nil)

	externalAccounts := dbmocks.NewMockUserExternalAccountsStore()
	externalAccounts.LookupUserAndSaveFunc.SetDefaultHook(m.LookupUserAndSave)
	externalAccounts.AssociateUserAndSaveFunc.SetDefaultHook(m.AssociateUserAndSave)
	externalAccounts.CreateUserAndSaveFunc.SetDefaultHook(m.CreateUserAndSave)

	users := dbmocks.NewMockUserStore()
	users.GetByIDFunc.SetDefaultHook(m.GetByID)
	users.GetByVerifiedEmailFunc.SetDefaultHook(m.GetByVerifiedEmail)
	users.GetByUsernameFunc.SetDefaultHook(m.GetByUsername)
	users.UpdateFunc.SetDefaultHook(m.Update)

	authzStore := dbmocks.NewMockAuthzStore()
	authzStore.GrantPendingPermissionsFunc.SetDefaultHook(m.GrantPendingPermissions)

	permsSyncStore := dbmocks.NewMockPermissionSyncJobStore()
	permsSyncStore.CreateUserSyncJobFunc.SetDefaultHook(m.CreateUserSyncJobFunc)

	db := dbmocks.NewMockDB()
	db.GlobalStateFunc.SetDefaultReturn(gss)
	db.UserExternalAccountsFunc.SetDefaultReturn(externalAccounts)
	db.UsersFunc.SetDefaultReturn(users)
	db.AuthzFunc.SetDefaultReturn(authzStore)
	db.EventLogsFunc.SetDefaultReturn(dbmocks.NewMockEventLogStore())
	db.PermissionSyncJobsFunc.SetDefaultReturn(permsSyncStore)
	return db
}

// LookupUserAndSave mocks database.ExternalAccounts.LookupUserAndSave
func (m *mocks) LookupUserAndSave(_ context.Context, spec extsvc.AccountSpec, data extsvc.AccountData) (userID int32, err error) {
	if m.lookupUserAndSaveErr != nil {
		return 0, m.lookupUserAndSaveErr
	}

	for _, u := range m.userInfos {
		for _, a := range u.extAccts {
			if a == spec {
				m.savedExtAccts[u.user.ID] = append(m.savedExtAccts[u.user.ID], spec)
				return u.user.ID, nil
			}
		}
	}
	return 0, &errcode.Mock{IsNotFound: true}
}

// CreateUserAndSave mocks database.ExternalAccounts.CreateUserAndSave
func (m *mocks) CreateUserAndSave(_ context.Context, newUser database.NewUser, spec extsvc.AccountSpec, data extsvc.AccountData) (createdUser *types.User, err error) {
	if m.createUserAndSaveErr != nil {
		return &types.User{}, m.createUserAndSaveErr
	}

	// Check if username already exists
	for _, u := range m.userInfos {
		if u.user.Username == newUser.Username {
			return &types.User{}, database.MockCannotCreateUserUsernameExistsErr
		}
	}
	// Check if email already exists
	for _, u := range m.userInfos {
		for _, email := range u.emails {
			if email == newUser.Email {
				return &types.User{}, database.MockCannotCreateUserEmailExistsErr
			}
		}
	}

	// Create user
	userID := m.nextUserID
	m.nextUserID++
	if _, ok := m.createdUsers[userID]; ok {
		m.t.Fatalf("user %v should not already exist", userID)
	}
	m.createdUsers[userID] = newUser

	// Save ext acct
	m.savedExtAccts[userID] = append(m.savedExtAccts[userID], spec)

	return &types.User{ID: userID}, nil
}

// AssociateUserAndSave mocks database.ExternalAccounts.AssociateUserAndSave
func (m *mocks) AssociateUserAndSave(_ context.Context, userID int32, spec extsvc.AccountSpec, data extsvc.AccountData) (err error) {
	if m.associateUserAndSaveErr != nil {
		return m.associateUserAndSaveErr
	}

	// Check if ext acct is associated with different user
	for _, u := range m.userInfos {
		for _, a := range u.extAccts {
			if a == spec && u.user.ID != userID {
				return errors.Errorf("unable to change association of external account from user %d to user %d (delete the external account and then try again)", u.user.ID, userID)
			}
		}
	}

	m.savedExtAccts[userID] = append(m.savedExtAccts[userID], spec)
	return nil
}

// GetByVerifiedEmail mocks database.Users.GetByVerifiedEmail
func (m *mocks) GetByVerifiedEmail(ctx context.Context, email string) (*types.User, error) {
	if m.getByVerifiedEmailErr != nil {
		return nil, m.getByVerifiedEmailErr
	}

	for _, u := range m.userInfos {
		for _, e := range u.emails {
			if e == email {
				return &u.user, nil
			}
		}
	}
	return nil, database.MockUserNotFoundErr
}

// GetByUsername mocks database.Users.GetByUsername
func (m *mocks) GetByUsername(ctx context.Context, username string) (*types.User, error) {
	if m.getByUsernameErr != nil {
		return nil, m.getByUsernameErr
	}

	for _, u := range m.userInfos {
		if u.user.Username == username {
			return &u.user, nil
		}
	}
	return nil, database.MockUserNotFoundErr
}

// GetByID mocks database.Users.GetByID
func (m *mocks) GetByID(ctx context.Context, id int32) (*types.User, error) {
	if m.getByIDErr != nil {
		return nil, m.getByIDErr
	}

	for _, u := range m.userInfos {
		if u.user.ID == id {
			return &u.user, nil
		}
	}
	return nil, database.MockUserNotFoundErr
}

// Update mocks database.Users.Update
func (m *mocks) Update(ctx context.Context, id int32, update database.UserUpdate) error {
	if m.updateErr != nil {
		return m.updateErr
	}

	_, err := m.GetByID(ctx, id)
	if err != nil {
		return err
	}

	// Save user
	m.updatedUsers[id] = append(m.updatedUsers[id], update)
	return nil
}

// GrantPendingPermissions mocks database.Authz.GrantPendingPermissions
func (m *mocks) GrantPendingPermissions(context.Context, *database.GrantPendingPermissionsArgs) error {
	m.calledGrantPendingPermissions = true
	return nil
}

func (m *mocks) CreateUserSyncJobFunc(context.Context, int32, database.PermissionSyncJobOpts) error {
	m.calledCreateUserSyncJob = true
	return nil
}

func ext(serviceType, serviceID, clientID, accountID string) extsvc.AccountSpec {
	return extsvc.AccountSpec{
		ServiceType: serviceType,
		ServiceID:   serviceID,
		ClientID:    clientID,
		AccountID:   accountID,
	}
}

func userProps(username, email string) database.NewUser {
	return database.NewUser{
		Username:        username,
		Email:           email,
		EmailIsVerified: true,
	}
}
