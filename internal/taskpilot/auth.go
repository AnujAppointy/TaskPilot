package taskpilot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionTTL = 12 * time.Hour

const signedSessionPrefix = "tps2_"

type signedSessionPayload struct {
	UserID    string `json:"uid"`
	ExpiresAt int64  `json:"exp"`
	Nonce     string `json:"nonce"`
}

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

func defaultNameFromEmail(email string) string {
	local := strings.TrimSpace(strings.Split(strings.ToLower(email), "@")[0])
	if local == "" {
		return "TaskPilot User"
	}
	parts := strings.FieldsFunc(local, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '+'
	})
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	out := strings.TrimSpace(strings.Join(parts, " "))
	if out == "" {
		return "TaskPilot User"
	}
	return out
}

func (s *Store) CreateUser(ctx context.Context, email, name, password, _ string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return User{}, userErr("validation", "email is required")
	}
	if strings.TrimSpace(name) == "" {
		name = defaultNameFromEmail(email)
	}
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	u := User{ID: newID("user"), Email: email, Name: name, Active: true, CreatedAt: now}
	_, err = s.exec(ctx, `INSERT INTO users (id,email,name,password_hash,active,created_at,last_seen_at) VALUES (?,?,?,?,?,?,NULL)`,
		u.ID, u.Email, u.Name, hash, 1, ts(u.CreatedAt))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "role") {
		_, err = s.exec(ctx, `INSERT INTO users (id,email,name,password_hash,role,active,created_at,last_seen_at) VALUES (?,?,?,?,?,?,?,NULL)`,
			u.ID, u.Email, u.Name, hash, "developer", 1, ts(u.CreatedAt))
	}
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, userErr("validation", "an account with this email already exists")
		}
		return User{}, err
	}
	return u, s.addEvent(ctx, "", u.ID, "user.created", map[string]any{"id": u.ID, "email": u.Email})
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.query(ctx, `SELECT id,email,name,active,created_at,last_seen_at FROM users ORDER BY created_at DESC`)
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
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &active, &created, &last); err != nil {
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

func (s *Store) UpdateUser(ctx context.Context, actorID, userID, name, _ string, active *bool) (User, error) {
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
	_, err = s.exec(ctx, `UPDATE users SET name=?, active=? WHERE id=?`, strings.TrimSpace(name), activeInt, userID)
	if err != nil {
		return User{}, err
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}
	return *u, s.addEvent(ctx, "", actorID, "user.updated", map[string]any{"id": userID, "active": activeInt == 1})
}

func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	var u User
	var created string
	var last sql.NullString
	var active int
	err := s.queryRow(ctx, `SELECT id,email,name,active,created_at,last_seen_at FROM users WHERE id=?`, id).
		Scan(&u.ID, &u.Email, &u.Name, &active, &created, &last)
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
	err := s.queryRow(ctx, `SELECT id,email,name,password_hash,active,created_at,last_seen_at FROM users WHERE email=?`, email).
		Scan(&u.ID, &u.Email, &u.Name, &passwordHash, &active, &created, &last)
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
	now := time.Now().UTC()
	expires := now.Add(sessionTTL)
	token := newUserSessionToken(userID, expires)
	_, err := s.exec(ctx, `INSERT INTO sessions (id,user_id,token_hash,created_at,expires_at,revoked_at) VALUES (?,?,?,?,?,NULL)`,
		newID("sess"), userID, hashToken(token), ts(now), ts(expires))
	return token, err
}

func (s *Store) VerifySession(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, userErr("unauthorized", "missing session")
	}
	var userID, email, name string
	err := s.queryRow(ctx, `SELECT users.id, users.email, users.name FROM sessions JOIN users ON users.id=sessions.user_id WHERE sessions.token_hash=? AND sessions.revoked_at IS NULL AND sessions.expires_at>? AND users.active=1`,
		hashToken(token), ts(time.Now().UTC())).Scan(&userID, &email, &name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if blocked, blockErr := s.sessionTokenKnownButInvalid(ctx, token); blockErr != nil {
				return Principal{}, blockErr
			} else if blocked {
				return Principal{}, userErr("unauthorized", "invalid session")
			}
			return s.verifySignedSessionFallback(ctx, token)
		}
		return Principal{}, err
	}
	return Principal{ID: userID, Kind: "user", UserID: userID, ActorID: userID, Email: email, Name: name}, nil
}

func (s *Store) sessionTokenKnownButInvalid(ctx context.Context, token string) (bool, error) {
	var id string
	err := s.queryRow(ctx, `SELECT id FROM sessions WHERE token_hash=?`, hashToken(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) verifySignedSessionFallback(ctx context.Context, token string) (Principal, error) {
	payload, ok := verifySignedSessionToken(token)
	if !ok {
		return Principal{}, userErr("unauthorized", "invalid session")
	}
	var email, name string
	var active int
	err := s.queryRow(ctx, `SELECT email,name,active FROM users WHERE id=?`, payload.UserID).Scan(&email, &name, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, userErr("unauthorized", "invalid session")
	}
	if err != nil {
		return Principal{}, err
	}
	if active != 1 {
		return Principal{}, userErr("unauthorized", "invalid session")
	}
	return Principal{ID: payload.UserID, Kind: "user", UserID: payload.UserID, ActorID: payload.UserID, Email: email, Name: name}, nil
}

func (s *Store) RevokeSession(ctx context.Context, token string) error {
	_, err := s.exec(ctx, `UPDATE sessions SET revoked_at=? WHERE token_hash=?`, ts(time.Now().UTC()), hashToken(token))
	return err
}

func newUserSessionToken(userID string, expires time.Time) string {
	if sessionSigningKey() == "" {
		return "tps_" + newSecret()
	}
	payload := signedSessionPayload{UserID: userID, ExpiresAt: expires.UTC().Unix(), Nonce: newSecret()}
	payloadBytes, _ := json.Marshal(payload)
	payloadPart := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := signSessionPayload(payloadPart)
	return signedSessionPrefix + payloadPart + "." + sig
}

func verifySignedSessionToken(token string) (signedSessionPayload, bool) {
	if sessionSigningKey() == "" || !strings.HasPrefix(token, signedSessionPrefix) {
		return signedSessionPayload{}, false
	}
	parts := strings.Split(strings.TrimPrefix(token, signedSessionPrefix), ".")
	if len(parts) != 2 {
		return signedSessionPayload{}, false
	}
	expected := signSessionPayload(parts[0])
	if !hmac.Equal([]byte(expected), []byte(parts[1])) {
		return signedSessionPayload{}, false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return signedSessionPayload{}, false
	}
	var payload signedSessionPayload
	if json.Unmarshal(payloadBytes, &payload) != nil || payload.UserID == "" {
		return signedSessionPayload{}, false
	}
	if time.Now().UTC().Unix() > payload.ExpiresAt {
		return signedSessionPayload{}, false
	}
	return payload, true
}

func signSessionPayload(payloadPart string) string {
	mac := hmac.New(sha256.New, []byte(sessionSigningKey()))
	_, _ = mac.Write([]byte(payloadPart))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func sessionSigningKey() string {
	key := strings.TrimSpace(os.Getenv("TASKPILOT_SECRET_KEY"))
	if len(key) < 32 {
		return ""
	}
	return key
}
