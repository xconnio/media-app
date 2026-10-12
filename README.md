# media-app

Serves deskconn's Pictures, Videos and Documents apps. It indexes the user's Desktop,
Downloads, Documents, Pictures and Videos folders (with thumbnails) and answers
deskconnd's index queries over deskconnd's local socket (`~/.deskconn/deskconn.sock`).

It registers these apps with deskconnd, which lists them for the frontend
(`capabilities.list`) while media-app is connected. Without media-app, they aren't shown.

Run it next to deskconnd, as the same user:

```
make build && ./media-app
```

Thumbnails for videos and PDFs need `ffmpeg` and `pdftocairo` (poppler-utils).
