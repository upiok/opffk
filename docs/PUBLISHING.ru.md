# Публикация: форк OpenFlux + свой opffk на GitHub

Пошаговая инструкция: как забрать себе форк OpenFlux, собрать и
опубликовать собственное ядро `node-v*`, привязать его к opffk и выложить
opffk с рабочей установкой в одну команду.

## 0. Что где лежит

| Что | Репозиторий | Зачем |
|---|---|---|
| Ядро OpenFlux (бинарники ноды) | `p1neappleXpress/OpenFlux` → ваш форк | Апстрим. Ваш форк нужен, чтобы собственные сборки ядра `node-v*` публиковались под вашим именем и opffk не зависел от чужого релизного цикла |
| opffk (этот репозиторий) | ваш новый репозиторий, например `ВАШ-ЛОГИН/opffk` | Установщик, веб-интерфейс, документация. Ядро не содержит — скачивает его по манифесту |

## 1. Форк OpenFlux

```sh
# через веб: https://github.com/p1neappleXpress/OpenFlux → Fork
# или через gh CLI:
gh repo fork p1neappleXpress/OpenFlux --clone=false
```

Важно понимать, что произойдёт дальше:

- В форке уже лежат все workflows апстрима, включая `node-release.yml`:
  пуш тега `node-vX.Y.Z` собирает ядро (`-tags exitnode`) для
  linux amd64/arm64/arm7 и публикует релиз с `SHA256SUMS`, сверяя хеши с
  пинами в `deploy/node-install.sh` (эта проверка в workflow есть — при
  своих изменениях кода нужно обновить и пины, иначе job упадёт; это
  защита, а не помеха).
- Для "просто взять текущий код и собрать под собой" ничего менять не
  нужно: достаточно создать тег у себя в форке.

## 2. Собственное ядро `node-v*` из форка

```sh
git clone https://github.com/ВАШ-ЛОГИН/OpenFlux
cd OpenFlux
git tag node-v1.3.1 edcf77a          # пример: закрепить конкретный коммит
git push origin node-v1.3.1
```

После пуша тега в форке отработает `node-release.yml` и в разделе
Releases появятся `openflux-linux-amd64`, `openflux-linux-arm64`,
`openflux-linux-arm` и `SHA256SUMS`. Дождитесь зелёной галочки воркфлоу.

> Совет: закрепляйте **конкретный коммит** (или существующий тег апстрима),
> а не «что сейчас в main» — воспроизводимость важнее свежести.

## 3. Привязать ядро форка к opffk

Отредактируйте `manifest.json` в opffk — три поля:

```json
"repository": "ВАШ-ЛОГИН/OpenFlux",
"url_template": "https://github.com/ВАШ-ЛОГИН/OpenFlux/releases/download/node-v1.3.1/openflux-linux-{arch}",
"sha256": { "amd64": "…", "arm64": "…", "arm": "…" }
```

Хеши возьмите из `SHA256SUMS` вашего релиза (или выполните у себя
`sha256sum openflux-linux-*` после сборки). Проверить привязку можно
локально:

```sh
sh scripts/build.sh --version v0.1.0 --core download   # скачает и сверит хеши
```

Сборка падает, если хотя бы один хеш не сходится — это ожидаемое поведение.

## 4. Опубликовать opffk

```sh
cd opffk                                            # этот репозиторий
git init && git add -A && git commit -m "opffk: initial release"
git remote add origin https://github.com/ВАШ-ЛОГИН/opffk.git
git push -u origin main
```

Дальше релиз делается тегом — `.github/workflows/release.yml` сам соберёт
три архитектуры (`--core download` = скачает ядро из вашего `manifest.json`
и сверит хеши), прогонит e2e-тесты и опубликует релиз с `.sha256`:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

(`workflow_dispatch` позволяет собрать артефакты без публикации.)

## 5. Проверить установку одной командой

После публикации релиза:

```sh
curl -fsSL https://raw.githubusercontent.com/ВАШ-ЛОГИН/opffk/main/install.sh \
  | OPFFK_REPO=ВАШ-ЛОГИН/opffk sh
```

Установщик скачает `opffk-v0.1.0-linux-<arch>.tar.gz` и его `.sha256` из
релиза, проверит их, внутри архива ещё раз сверит ядро с `manifest.json` и
только потом установит. Ручной путь — в README (раздел «Ручная установка»).

## 6. Обновления ядра на живых нодах

- Вариант А (ручной): выпускаете новый тег opffk с обновлённым
  `manifest.json`, на роутере повторно запускаете `install.sh` (он
  идемпотентен, конфигурация сохраняется).
- Вариант Б (из веб-интерфейса): в `ui.json` задать
  `update_manifest_url` — HTTPS-ссылку на `manifest.json` (например,
  `https://raw.githubusercontent.com/ВАШ-ЛОГИН/opffk/main/manifest.json`).
  Кнопки *Check / Update / Roll back* в карточке *Core updates* начнут
  работать: скачивание только по HTTPS, только по хешам из манифеста,
  автоматический откат, если обновлённое ядро не поднялось.

## 7. Чек-лист перед публикацией

- [ ] `git config user.name/user.email` — ваши данные (в этом клоне они
      выставлены локально как заглушка — замените перед пушем:
      `git config user.name "Имя"; git config user.email "почта"`).
- [ ] Замените `ВАШ-ЛОГИН`/`ВАШ-РЕПО` в README.md/README.ru.md на
      фактические имена репозиториев.
- [ ] Тег opffk совпадает с `OPFFK_VERSION_DEFAULT` в `install.sh`
      (или пользователи задают `--version`).
- [ ] Релиз ядра в форке собрался зелёным, хеши в `manifest.json` совпадают.
- [ ] `sh scripts/build.sh --core download` отработал локально без ошибок.
- [ ] Лицензии: opffk — GPL-3.0-or-later (`LICENSE`, `NOTICE` уже в репо);
      ядро остаётся под GPL-3.0 апстрима.
