# Vislist

Вишлист на Go + React/TypeScript + Postgres.

## Запуск

```powershell
docker compose up -d --build
```

Приложение: http://localhost:8081

Админ:

- логин: `birthday_admin`
- пароль: `Wish-2026-Admin`

Данные Postgres и загруженные фото лежат на диске `E:`:

- `E:\vislist-data\postgres`
- `E:\vislist-data\uploads`

## Роли

- Гости регистрируются или входят по логину и паролю.
- Гость бронирует свободные подарки и видит свои брони.
- Админ добавляет подарки с фото и ссылками, а у занятых подарков видит только статус `Занято`.
