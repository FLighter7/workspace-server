# workspace-server

A small program you run on your own computer: it keeps each app's data as one JSON file on your disk and syncs that app's devices over WebRTC on your local network.

Небольшая программа, которую вы запускаете на своём компьютере: она хранит данные каждого приложения одним JSON-файлом на вашем диске и синхронизирует устройства этого приложения по WebRTC в локальной сети.

## Documentation

- **[User guide](docs/guide.md)** · **[Руководство пользователя](docs/guide.ru.md)**: what it is and why; installing, running, pairing a device, updating and removing it.
- [Architecture](docs/architecture.md): how it works, why it was built this way, its limits, what has been verified.
- [Protocol](docs/protocol.md): the contract between the server and an app.
- [Client package](client/README.md): `@flighter7/workspace-client`, the client a TypeScript app syncs through.
- [TODO](docs/TODO.md): what is left to do and to check.

Downloads: [GitHub Releases](https://github.com/FLighter7/workspace-server/releases).
