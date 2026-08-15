---
title: A real application
description: The persistence layer of a running service, taken from its source rather than invented.
---

Every other page here was written to explain something. This one was written to
ship, and is reproduced from the repository layer of
[devbubble-api](https://github.com/AlexAli29/devbubble-api/tree/orm/internal/repository) —
a chat backend with users, tags, private chats, messages and emailed sign-in
codes. It is worth reading because the shapes are the ones a real schema
produced, and because several of them are the answer to a question the rest of
these docs raise without settling.

## The schema, declared

Managed mode: the entities own the schema and migrations are planned from them.

```go
//orm:table public.users
type User struct {
    ID          uuid.UUID `orm:"pk,pgtype:uuid,default:gen_random_uuid()"`
    CreatedAt   time.Time `orm:"pgtype:timestamptz,default:now()"`
    Email       string    `orm:"unique"`
    Description *string
    Name        string
    IsVerified  bool `orm:"default:false"`

    AuthCodes    orm.Many[AuthCode]
    Tags         orm.Many[UserUserTag]
    Messages     orm.Many[Message]
    Participants orm.Many[ChatParticipant]
}
```

`Description` is a pointer and every other field is not, which is the whole
nullability story for this table. `gen_random_uuid()` and `now()` are the
database's defaults rather than values Go computes.

### Two foreign keys to the same table

A follow has a follower and a followee, both users. The column name comes from
the field, but the *constraint* cannot be derived — two candidates on one table
are indistinguishable without being told:

```go
//orm:table public.user_follows
type UserFollow struct {
    FollowerID uuid.UUID `orm:"pk,pgtype:uuid"`
    FolloweeID uuid.UUID `orm:"pk,pgtype:uuid"`

    Follower orm.One[User] `orm:"fk:user_follows_follower_id_fkey,ondelete:cascade"`
    Followee orm.One[User] `orm:"fk:user_follows_followee_id_fkey,ondelete:cascade"`
}
```

This is what `fk:` is for. Without it the generator has two relations pointing
at the same table and no way to say which constraint each one means.

### A composite key, and the index the other query needs

```go
//orm:table public.chat_participants
//orm:index chat_participants_user_idx (UserID)
type ChatParticipant struct {
    ChatID uuid.UUID `orm:"pk,pgtype:uuid"`
    UserID uuid.UUID `orm:"pk,pgtype:uuid"`

    Chat orm.One[PrivateChat] `orm:"ondelete:cascade"`
    User orm.One[User]        `orm:"ondelete:cascade"`
}
```

The primary key is `(chat_id, user_id)`, so finding a chat's participants is
indexed and listing one user's chats is not. The second query is the chat list,
which is why the index exists.

### Cascades, declared where the relation is

```go
//orm:table public.messages
//orm:index messages_chat_created_idx (ChatID, CreatedAt)
type Message struct {
    ID        uuid.UUID `orm:"pk,pgtype:uuid,default:gen_random_uuid()"`
    Text      string
    UserID    uuid.UUID `orm:"pgtype:uuid"`
    ChatID    uuid.UUID `orm:"pgtype:uuid"`
    CreatedAt time.Time `orm:"pgtype:timestamptz,default:now()"`

    User orm.One[User]        `orm:"ondelete:cascade"`
    Chat orm.One[PrivateChat] `orm:"ondelete:cascade"`
}
```

## Wiring

One pool, one handle, every repository bound to it:

```go
type Repositories struct {
    Users     *UserRepo
    AuthCodes *AuthCodeRepo
    Chats     *ChatRepo
    Messages  *MessageRepo
    Tags      *TagRepo
}

func NewRepositories(pool *pgxpool.Pool) *Repositories {
    return newRepositories(New(pool))
}

// Binding to any executor is what lets the tests bind to a transaction they
// roll back.
func newRepositories(db *DB) *Repositories {
    return &Repositories{
        Users:     NewUserRepo(db),
        AuthCodes: NewAuthCodeRepo(db),
        Chats:     NewChatRepo(db),
        Messages:  NewMessageRepo(db),
        Tags:      NewTagRepo(db),
    }
}
```

### One error translation, at the edge

```go
func wrapNotFound(what string, err error) error {
    if errors.Is(err, orm.ErrNotFound) {
        return fmt.Errorf("%s: %w", what, core.ErrNotFound)
    }
    return fmt.Errorf("%s: %w", what, err)
}
```

Nothing above this package imports `orm` to learn a row was missing.

## Reading

### A row, and a not-found that means something

```go
func (r *UserRepo) ByEmail(ctx context.Context, email string) (core.User, error) {
    user, err := r.db.Users.Query().
        Where(Users.Email.Eq(email)).
        One(ctx)
    if err != nil {
        return core.User{}, wrapNotFound("get user by email", err)
    }
    return toCoreUser(user), nil
}
```

### A relation two levels deep

The tags a user has taken, through the link table:

```go
user, err := r.db.Users.Query().
    Where(Users.ID.Eq(userID)).
    With(Users.Tags.With(UserUserTags.Tag)).
    One(ctx)
if err != nil {
    return core.User{}, nil, wrapNotFound("get user", err)
}

links, _ := user.Tags.Get()
tags := make([]core.Tag, 0, len(links))
for _, link := range links {
    tag, ok := link.Tag.Get()
    if !ok || tag == nil {
        continue
    }
    tags = append(tags, toCoreTag(*tag))
}
```

`Get` returns the loaded value and whether it was loaded, which is how an
unloaded relation is told apart from an empty one. This replaced a `json_agg`
blob the caller unmarshalled.

### An anti-join, without loading the other side

The tags this user has *not* taken yet:

```go
tags, err := r.db.UserTags.Query().
    Where(UserTags.Users.None(UserUserTags.UserID.Eq(user))).
    All(ctx)
```

`None` filters by the absence of a link row. Nothing about the link table is
selected, and no second query runs.

### The newest child per parent, in one statement

The chat list: every chat the caller is in, with the other participant and the
most recent message in each.

```go
participations, err := r.db.ChatParticipants.Query().
    Where(ChatParticipants.UserID.Eq(user)).
    With(ChatParticipants.Chat.
        With(PrivateChats.Participants.
            Where(ChatParticipants.UserID.Ne(user)).
            With(ChatParticipants.User)).
        With(PrivateChats.Messages.
            OrderBy(Messages.CreatedAt.Desc()).
            Limit(1).
            With(Messages.User))).
    All(ctx)
```

Five statements whatever the number of chats. The `Limit(1)` is *per chat*,
which is what makes the newest message one statement rather than one per chat —
and it is the reason this shape is worth the nesting.

### A projection instead of a relation, and why

Chat history needs five values, one of which lives on `users`. `With(Messages.User)`
would also be one statement — a to-one relation compiles to a `LEFT JOIN` — but
it selects all six user columns for every message to read one name off each.

```go
shape := orm.Project5(
    orm.Of(Messages.ID),
    orm.Of(Messages.Text),
    orm.Of(Messages.CreatedAt),
    orm.Of(Messages.UserID),
    orm.Of(Users.Name),
    func(id uuid.UUID, text string, createdAt time.Time, userID uuid.UUID, senderName string) core.ChatMessage {
        return core.ChatMessage{
            Id:         id.String(),
            Text:       text,
            CreatedAt:  createdAt,
            UserId:     userID.String(),
            SenderName: senderName,
            IsFromMe:   userID == viewer,
        }
    },
)

out, err := orm.Compose(r.db.Executor(), shape).
    From(Messages.Source()).
    Join(Users.Source(), orm.Eq(Messages.UserID, Users.ID)).
    Where(orm.Cond(Messages.ChatID.Eq(chat))).
    OrderBy(orm.Of(Messages.CreatedAt).Asc()).
    All(ctx)
```

The join is inner because `messages.user_id` is `NOT NULL` and references
`users`, so it cannot drop a row. That is a fact about the schema, and the
schema is where it was checked.

Note where the two shapes each earn their place: the chat *list* keeps the
relation load, because its cost is bounded by the number of chats. The chat
*history* does not, because its cost is bounded by the number of messages.

## Writing

### Insert, letting the database fill in what it owns

```go
user, err := r.db.Users.Insert(ctx, User{
    Name:  name,
    Email: email,
}, orm.Default(Users.ID, Users.CreatedAt, Users.IsVerified))
```

`IsVerified` would otherwise be stored as `false` — a Go zero value is a value.
Asking for the column default is the separate, explicit call.

### A partial update, built from what was supplied

The real answer to "update only the fields that were sent", which used to be a
`COALESCE(NULLIF(...))` in SQL:

```go
assignments := make([]orm.Assign[User], 0, 2)
if name != "" {
    assignments = append(assignments, Users.Name.Set(name))
}
if description != "" {
    assignments = append(assignments, Users.Description.Set(description))
}
if len(assignments) == 0 {
    return nil
}

if _, err := r.db.Users.Update().
    Set(assignments...).
    Where(Users.ID.Eq(userID)).
    Exec(ctx); err != nil {
    return fmt.Errorf("update user: %w", err)
}
```

`Set` is variadic over `orm.Assign[E]`, so the set of columns is decided at run
time and the types are still decided at compile time.

### An upsert whose no-op is the success case

```go
_, err = r.db.UserUserTags.Insert(ctx, UserUserTag{
    UserID: user,
    TagID:  tag,
}, orm.OnConflict(UserUserTags.UserID, UserUserTags.TagID).DoNothing())

if err != nil && !errors.Is(err, orm.ErrConflictIgnored) {
    return fmt.Errorf("add tag: %w", err)
}
```

`DO NOTHING` returns no row, and the ORM reports that rather than hiding it.
Here it means the user already has the tag — which is what was asked for, so the
sentinel is caught and the call succeeds.

### An ownership check that cannot race

```go
if _, err := r.db.Messages.Delete().
    Where(Messages.ID.Eq(message)).
    Where(Messages.UserID.Eq(user)).
    Exec(ctx); err != nil {
    return fmt.Errorf("remove message: %w", err)
}
```

The check is in the `WHERE`, so there is no window between reading the owner and
deleting the row.

### A cascade doing the work

```go
if _, err := tx.PrivateChats.Delete().
    Where(PrivateChats.ID.Eq(chat)).
    Exec(ctx); err != nil {
    return fmt.Errorf("remove chat: %w", err)
}
```

One statement against `private_chats`. The messages and participants go with it,
because both foreign keys are declared `ON DELETE CASCADE` — this replaced a
hand-rolled deletion of each child table in order.

## Transactions

### Two writes that must both happen

```go
if err := tx(ctx, r.db, func(tx *DB) error {
    chat, err := tx.PrivateChats.Insert(ctx, PrivateChat{ID: uuid.New()})
    if err != nil {
        return fmt.Errorf("create chat: %w", err)
    }
    chatID = chat.ID

    if _, err := tx.ChatParticipants.InsertMany(ctx, []ChatParticipant{
        {ChatID: chat.ID, UserID: first},
        {ChatID: chat.ID, UserID: second},
    }); err != nil {
        return fmt.Errorf("add chat participants: %w", err)
    }
    return nil
}); err != nil {
    return "", err
}
```

`private_chats` has no column but its key, so there is nothing to default and
the id is generated in Go.

### A conditional delete that reports what it did

```go
err = tx(ctx, r.db, func(tx *DB) error {
    isParticipant, err := tx.ChatParticipants.Query().
        Where(ChatParticipants.ChatID.Eq(chat)).
        Where(ChatParticipants.UserID.Eq(user)).
        Exists(ctx)
    if err != nil {
        return fmt.Errorf("check chat participant: %w", err)
    }
    if !isParticipant {
        return nil
    }

    if _, err := tx.PrivateChats.Delete().
        Where(PrivateChats.ID.Eq(chat)).
        Exec(ctx); err != nil {
        return fmt.Errorf("remove chat: %w", err)
    }

    removed = true
    return nil
})
```

`Exists` asks the question without reading the row. The caller can tell "not
allowed" from "already gone" because the boolean is returned separately from the
error.

### Predicates decided at run time

Matching a sign-in code: the same query, with one extra condition when the
account must already be verified.

```go
match := []orm.Predicate[User]{Users.Email.Eq(email)}
if requireVerified {
    match = append(match, Users.IsVerified.Eq(true))
}

authCode, err := tx.AuthCodes.Query().
    Where(AuthCodes.Code.Eq(code)).
    Where(AuthCodes.UpdatedAt.Gt(time.Now().Add(-authCodeTTL))).
    Where(AuthCodes.User.Any(match...)).
    With(AuthCodes.User).
    One(ctx)
```

A slice of `orm.Predicate[User]` passed into `Any` — a filter on the *related*
table, composed at run time, still checked against `User` at compile time. A
`Predicate[Message]` in that slice does not build.

### Accept and rotate, indivisibly

```go
if markVerified {
    if _, err := tx.Users.Update().
        Set(Users.IsVerified.Set(true)).
        Where(Users.ID.Eq(userID)).
        Exec(ctx); err != nil {
        return fmt.Errorf("mark user verified: %w", err)
    }
}

if _, err := tx.AuthCodes.Update().
    Set(AuthCodes.Code.Set(next)).
    Set(AuthCodes.UpdatedAt.Set(time.Now())).
    Where(AuthCodes.ID.Eq(authCode.ID)).
    Exec(ctx); err != nil {
    return fmt.Errorf("rotate auth code: %w", err)
}
```

A code that has been accepted must not stay acceptable, so matching it and
replacing it are one transaction. That is a persistence concern, which is why it
lives in this layer rather than in the service above it.
