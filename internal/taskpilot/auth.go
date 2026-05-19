package taskpilot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionTTL = 12 * time.Hour

func hashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", userErr("validation", "password must be at least 8 characters")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func verifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func hashToken(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func parseScopes(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinScopes(v []string) string {
	return strings.Join(v, ",")
}

func hasScope(scopes []string, required string) bool {
	for _, scope := range scopes {
		if scope == "admin" || scope == required {
			return true
		}
	}
	return false
}

func roleAllows(role, required string) bool {
	if required == "read" {
		return oneOf(role, "admin", "maintainer", "developer", "agent", "viewer")
	}
	if required == "write" {
		return oneOf(role, "admin", "maintainer", "developer", "agent")
	}
	if required == "admin" {
		return role == "admin"
	}
	return false
}

func (s *Store) CreateUser(ctx context.Context, email, name, password, role string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return User{}, userErr("validation", "email is required")
	}
	if strings.TrimSpace(name) == "" {
		return User{}, userErr("validation", "name is required")
	}
	if role == "" {
		role = "developer"
	}
	if !oneOf(role, "admin", "maintainer", "developer", "viewer") {
		return User{}, userErr("validation", "invalid user role")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	u := User{ID: newID("user"), Email: email, Name: name, Role: role, Active: true, CreatedAt: now}
	_, err = s.exec(ctx, `INSERT INTO users (id,email,name,password_hash,role,active,created_at,last_seen_at) VALUES (?,?,?,?,?,?,?,NULL)`,
		u.ID, u.Email, u.Name, hash, u.Role, 1, ts(u.CreatedAt))
	if err != nil {
		return User{}, err
	}
	return u, s.addEvent(ctx, "", u.ID, "user.created", map[string]any{"id": u.ID, "email": u.Email, "role": u.Role})
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.query(ctx, `SELECT id,email,name,role,active,created_at,last_seen_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var created string
		var last sql.NullString
		var active int
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &active, &created, &last); err != nil {
			return nil, err
		}
		u.Active = active == 1
		u.CreatedAt = parseTS(created)
		if last.Valid {
			t := parseTS(last.String)
			u.LastSeenAt = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(ctx context.Context, actorID, userID, name, role string, active *bool) (User, error) {
	current, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if current == nil {
		return User{}, userErr("not_found", "user not found")
	}
	if strings.TrimSpace(name) == "" {
		name = current.Name
	}
	if role == "" {
		role = current.Role
	}
	if !oneOf(role, "admin", "maintainer", "developer", "viewer") {
		return User{}, userErr("validation", "invalid user role")
	}
	activeInt := 0
	if current.Active {
		activeInt = 1
	}
	if active != nil {
		if *active {
			activeInt = 1
		} else {
			activeInt = 0
		}
	}
	_, err = s.exec(ctx, `UPDATE users SET name=?, role=?, active=? WHERE id=?`, strings.TrimSpace(name), role, activeInt, userID)
	if err != nil {
		return User{}, err
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}
	return *u, s.addEvent(ctx, "", actorID, "user.updated", map[string]any{"id": userID, "role": role, "active": activeInt == 1})
}

func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	var u User
	var created string
	var last sql.NullString
	var active int
	err := s.queryRow(ctx, `SELECT id,email,name,role,active,created_at,last_seen_at FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Email, &u.Name, &u.Role, &active, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Active = active == 1
	u.CreatedAt = parseTS(created)
	if last.Valid {
		t := parseTS(last.String)
		u.LastSeenAt = &t
	}
	return &u, nil
}

func (s *Store) ChangeUserPassword(ctx context.Context, actorID, userID, currentPassword, newPassword string, requireCurrent bool) error {
	var passwordHash string
	err := s.queryRow(ctx, `SELECT password_hash FROM users WHERE id=? AND active=1`, userID).Scan(&passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return userErr("not_found", "active user not found")
	}
	if err != nil {
		return err
	}
	if requireCurrent && !verifyPassword(passwordHash, currentPassword) {
		return userErr("unauthorized", "current password is invalid")
	}
	hash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID)
	if err != nil {
		return err
	}
	_, _ = s.exec(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, ts(time.Now().UTC()), userID)
	return s.addEvent(ctx, "", actorID, "user.password_changed", map[string]any{"id": userID})
}

func (s *Store) AuthenticateUser(ctx context.Context, email, password string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var u User
	var passwordHash, created string
	var last sql.NullString
	var active int
	err := s.queryRow(ctx, `SELECT id,email,name,password_hash,role,active,created_at,last_seen_at FROM users WHERE email=?`, email).
		Scan(&u.ID, &u.Email, &u.Name, &passwordHash, &u.Role, &active, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, userErr("unauthorized", "invalid email or password")
	}
	if err != nil {
		return User{}, err
	}
	if active != 1 || !verifyPassword(passwordHash, password) {
		return User{}, userErr("unauthorized", "invalid email or password")
	}
	u.Active = true
	u.CreatedAt = parseTS(created)
	if last.Valid {
		t := parseTS(last.String)
		u.LastSeenAt = &t
	}
	now := time.Now().UTC()
	_, _ = s.exec(ctx, `UPDATE users SET last_seen_at=? WHERE id=?`, ts(now), u.ID)
	u.LastSeenAt = &now
	return u, nil
}

func (s *Store) CreateSession(ctx context.Context, userID string) (string, error) {
	token := "tps_" + newSecret()
	now := time.Now().UTC()
	expires := now.Add(sessionTTL)
	_, err := s.exec(ctx, `INSERT INTO sessions (id,user_id,token_hash,created_at,expires_at,revoked_at) VALUES (?,?,?,?,?,NULL)`,
		newID("sess"), userID, hashToken(token), ts(now), ts(expires))
	return token, err
}

func (s *Store) VerifySession(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, userErr("unauthorized", "missing session")
	}
	var userID, role string
	err := s.queryRow(ctx, `SELECT users.id, users.role FROM sessions JOIN users ON users.id=sessions.user_id WHERE sessions.token_hash=? AND sessions.revoked_at IS NULL AND sessions.expires_at>? AND users.active=1`,
		hashToken(token), ts(time.Now().UTC())).Scan(&userID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, userErr("unauthorized", "invalid session")
	}
	if err != nil {
		return Principal{}, err
	}
	return Principal{ID: userID, Kind: "user", Role: role, UserID: userID, ActorID: userID}, nil
}

func (s *Store) RevokeSession(ctx context.Context, token string) error {
	_, err := s.exec(ctx, `UPDATE sessions SET revoked_at=? WHERE token_hash=?`, ts(time.Now().UTC()), hashToken(token))
	return err
}

func (s *Store) CreateAPIKey(ctx context.Context, name, actorID, role string, scopes []string, createdBy string) (APIKey, error) {
	if name == "" {
		return APIKey{}, userErr("validation", "api key name is required")
	}
	if actorID == "" {
		return APIKey{}, userErr("validation", "actor_id is required")
	}
	actor, err := s.GetActor(ctx, actorID)
	if err != nil {
		return APIKey{}, err
	}
	if actor == nil {
		return APIKey{}, userErr("validation", "actor_id does not exist")
	}
	if role == "" {
		role = "agent"
	}
	if !oneOf(role, "admin", "maintainer", "developer", "agent", "viewer") {
		return APIKey{}, userErr("validation", "invalid api key role")
	}
	if len(scopes) == 0 {
		scopes = []string{"task:read", "task:write", "lock:write", "context:write", "handoff:write"}
	}
	for _, scope := range scopes {
		if !oneOf(scope, "task:read", "task:write", "lock:write", "context:write", "handoff:write", "artifact:request", "admin") {
			return APIKey{}, userErr("validation", "invalid api key scope")
		}
		if scope == "admin" && role != "admin" {
			return APIKey{}, userErr("validation", "admin scope requires admin api key role")
		}
	}
	secret := "tpk_" + newSecret()
	prefix := secret
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	now := time.Now().UTC()
	key := APIKey{ID: newID("key"), Name: name, ActorID: actorID, Role: role, Scopes: scopes, Prefix: prefix, Secret: secret, CreatedBy: createdBy, CreatedAt: now}
	_, err = s.exec(ctx, `INSERT INTO api_keys (id,name,actor_id,role,scopes,key_hash,prefix,created_by,created_at,revoked_at) VALUES (?,?,?,?,?,?,?,?,?,NULL)`,
		key.ID, key.Name, key.ActorID, key.Role, joinScopes(key.Scopes), hashToken(secret), key.Prefix, key.CreatedBy, ts(key.CreatedAt))
	if err != nil {
		return APIKey{}, err
	}
	return key, s.addEvent(ctx, "", createdBy, "api_key.created", map[string]any{"id": key.ID, "name": key.Name, "actor_id": key.ActorID, "scopes": key.Scopes})
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.query(ctx, `SELECT id,name,actor_id,role,scopes,prefix,created_by,created_at,revoked_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var key APIKey
		var scopes, created string
		var revoked sql.NullString
		if err := rows.Scan(&key.ID, &key.Name, &key.ActorID, &key.Role, &scopes, &key.Prefix, &key.CreatedBy, &created, &revoked); err != nil {
			return nil, err
		}
		key.Scopes = parseScopes(scopes)
		key.CreatedAt = parseTS(created)
		if revoked.Valid {
			t := parseTS(revoked.String)
			key.RevokedAt = &t
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, actorID, keyID string) error {
	res, err := s.exec(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, ts(time.Now().UTC()), keyID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return userErr("not_found", "active api key not found")
	}
	return s.addEvent(ctx, "", actorID, "api_key.revoked", map[string]any{"id": keyID})
}

func (s *Store) VerifyAPIKey(ctx context.Context, secret string) (Principal, error) {
	if secret == "" {
		return Principal{}, userErr("unauthorized", "missing api key")
	}
	var key APIKey
	var scopes, created string
	err := s.queryRow(ctx, `SELECT id,name,actor_id,role,scopes,prefix,created_by,created_at FROM api_keys WHERE key_hash=? AND revoked_at IS NULL`,
		hashToken(secret)).Scan(&key.ID, &key.Name, &key.ActorID, &key.Role, &scopes, &key.Prefix, &key.CreatedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, userErr("unauthorized", "invalid api key")
	}
	if err != nil {
		return Principal{}, err
	}
	key.Scopes = parseScopes(scopes)
	return Principal{ID: key.ID, Kind: "api_key", Role: key.Role, ActorID: key.ActorID, Scopes: key.Scopes}, nil
}
