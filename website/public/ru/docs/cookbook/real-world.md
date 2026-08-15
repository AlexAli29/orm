# Настоящее приложение

> Слой персистентности работающего сервиса — взят из исходников, а не придуман.

Source: https://ormgo.vercel.app/ru/docs/cookbook/real-world/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
Все остальные страницы здесь написаны, чтобы что-то объяснить. Эта написана, чтобы работать в проде, и воспроизведена из слоя репозиториев проекта [devbubble-api](https://github.com/AlexAli29/devbubble-api/tree/orm/internal/repository) — бэкенда чата с пользователями, тегами, приватными чатами, сообщениями и кодами входа по почте. Её стоит прочитать, потому что здесь формы, которые породила настоящая схема, и потому что некоторые из них — ответ на вопрос, который остальная документация ставит, но не закрывает.

## Схема, объявленная

Управляемый режим: схемой владеют сущности, миграции планируются от них.

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

`Description` — указатель, остальные поля — нет; вот и вся история nullable-ности для этой таблицы. `gen_random_uuid()` и `now()` — умолчания базы, а не значения, которые вычисляет Go.

### Два внешних ключа на одну таблицу

У подписки есть подписчик и тот, на кого подписаны, и оба — пользователи. Имя колонки выводится из поля, а вот *ограничение* вывести нельзя: два кандидата на одной таблице неразличимы, пока не сказать прямо:

```go
//orm:table public.user_follows
type UserFollow struct {
    FollowerID uuid.UUID `orm:"pk,pgtype:uuid"`
    FolloweeID uuid.UUID `orm:"pk,pgtype:uuid"`

    Follower orm.One[User] `orm:"fk:user_follows_follower_id_fkey,ondelete:cascade"`
    Followee orm.One[User] `orm:"fk:user_follows_followee_id_fkey,ondelete:cascade"`
}
```

Ровно для этого и нужен `fk:`. Без него у генератора две связи, указывающие на одну таблицу, и никакого способа понять, какое ограничение имеется в виду.

### Составной ключ и индекс, который нужен другому запросу

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

Первичный ключ — `(chat_id, user_id)`, поэтому поиск участников чата индексирован, а список чатов одного пользователя — нет. Второй запрос и есть список чатов, ради него индекс и существует.

### Каскады, объявленные там же, где связь

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

## Сборка

Один пул, один хэндл, все репозитории привязаны к нему:

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

### Один перевод ошибок, на границе

```go
func wrapNotFound(what string, err error) error {
    if errors.Is(err, orm.ErrNotFound) {
        return fmt.Errorf("%s: %w", what, core.ErrNotFound)
    }
    return fmt.Errorf("%s: %w", what, err)
}
```

Ничто выше этого пакета не импортирует `orm`, чтобы узнать, что строки не нашлось.

## Чтение

### Строка и «не найдено», которое что-то значит

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

### Связь на два уровня вглубь

Теги, которые взял пользователь, — через таблицу связи:

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

`Get` возвращает загруженное значение и признак того, загружали ли его вообще: именно так незагруженная связь отличается от пустой. Это заменило blob из `json_agg`, который разбирал вызывающий код.

### Антисоединение, без загрузки другой стороны

Теги, которые этот пользователь ещё *не* взял:

```go
tags, err := r.db.UserTags.Query().
    Where(UserTags.Users.None(UserUserTags.UserID.Eq(user))).
    All(ctx)
```

`None` фильтрует по отсутствию строки связи. Ничего из таблицы связи не выбирается, и второй запрос не выполняется.

### Самый свежий потомок на родителя, одним запросом

Список чатов: все чаты, где состоит вызывающий, с другим участником и самым свежим сообщением в каждом.

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

Пять запросов при любом числе чатов. `Limit(1)` действует *на каждый чат* — именно это делает «самое свежее сообщение» одним запросом, а не одним на чат, и ради этого вложенность и оправдана.

### Проекция вместо связи, и почему

Истории чата нужны пять значений, одно из которых лежит в `users`. `With(Messages.User)` тоже был бы одним запросом — связь «к одному» компилируется в `LEFT JOIN`, — но он выбирает все шесть колонок пользователя на каждое сообщение ради одного имени.

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

Соединение внутреннее, потому что `messages.user_id` — `NOT NULL` и ссылается на `users`, так что строку оно потерять не может. Это факт о схеме, и проверен он там же, в схеме.

Обратите внимание, где какая форма оправдана: *список* чатов оставляет загрузку связей, потому что его стоимость ограничена числом чатов. *История* чата — нет, потому что её стоимость ограничена числом сообщений.

## Запись

### Вставка, с умолчаниями, которыми владеет база

```go
user, err := r.db.Users.Insert(ctx, User{
    Name:  name,
    Email: email,
}, orm.Default(Users.ID, Users.CreatedAt, Users.IsVerified))
```

Иначе `IsVerified` сохранился бы как `false`: нулевое значение Go — это значение. Попросить умолчание колонки — отдельный явный вызов.

### Частичное обновление, собранное из того, что прислали

Настоящий ответ на «обновить только присланные поля», который раньше был `COALESCE(NULLIF(...))` в SQL:

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

`Set` вариадичен по `orm.Assign[E]`, поэтому набор колонок решается во время выполнения, а типы по-прежнему решаются при компиляции.

### Upsert, у которого «ничего не сделал» и есть успех

```go
_, err = r.db.UserUserTags.Insert(ctx, UserUserTag{
    UserID: user,
    TagID:  tag,
}, orm.OnConflict(UserUserTags.UserID, UserUserTags.TagID).DoNothing())

if err != nil && !errors.Is(err, orm.ErrConflictIgnored) {
    return fmt.Errorf("add tag: %w", err)
}
```

`DO NOTHING` не возвращает строки, и библиотека об этом сообщает, а не скрывает. Здесь это значит, что тег у пользователя уже есть, — то есть ровно то, чего просили, поэтому сигнальная ошибка ловится и вызов считается успешным.

### Проверка владения, которую нельзя проиграть в гонке

```go
if _, err := r.db.Messages.Delete().
    Where(Messages.ID.Eq(message)).
    Where(Messages.UserID.Eq(user)).
    Exec(ctx); err != nil {
    return fmt.Errorf("remove message: %w", err)
}
```

Проверка стоит в `WHERE`, поэтому между чтением владельца и удалением строки нет окна.

### Каскад, делающий работу

```go
if _, err := tx.PrivateChats.Delete().
    Where(PrivateChats.ID.Eq(chat)).
    Exec(ctx); err != nil {
    return fmt.Errorf("remove chat: %w", err)
}
```

Один запрос к `private_chats`. Сообщения и участники уходят вместе с ним, потому что оба внешних ключа объявлены `ON DELETE CASCADE`, — это заменило ручное удаление каждой дочерней таблицы по порядку.

## Транзакции

### Две записи, которые должны произойти обе

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

У `private_chats` нет колонок, кроме ключа, поэтому умолчанию неоткуда взяться и идентификатор порождается в Go.

### Условное удаление, которое сообщает, что сделало

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

`Exists` задаёт вопрос, не читая строку. Вызывающий отличит «нельзя» от «уже удалено», потому что булево значение возвращается отдельно от ошибки.

### Предикаты, решаемые во время выполнения

Сопоставление кода входа: тот же запрос, но с дополнительным условием, когда аккаунт уже должен быть подтверждён.

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

Срез `orm.Predicate[User]`, переданный в `Any`, — фильтр по *связанной* таблице, собранный во время выполнения и всё равно проверенный против `User` при компиляции. `Predicate[Message]` в этом срезе не соберётся.

### Принять и заменить, неделимо

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

Принятый код не должен остаться принимаемым, поэтому сопоставление и замена — одна транзакция. Это забота слоя персистентности, поэтому она живёт здесь, а не в сервисе выше.
