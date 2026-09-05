# Repository Guidelines

## プロジェクト概要

`dam` は Unix パイプラインの起動ゲートです。v0.4.0 の CLI は `dam CONDITION [--or CONDITION]... [--buffer-size SIZE]`、単独指定の `dam --version`、および `-h` / `--help` を受け付けます。v0.4.0 では条件の構文が breaking change になり、release condition はすべて prefix 付きの positional argument で指定します。duration、datetime、signal、file の条件は OR で組み合わせ、1つの argument 内の ` && ` は AND group を表します。help と version は stdin を読まずに stdout へ出力して終了します。

現在の実装では、次の契約を維持してください。

- `CONDITION` は `duration:DURATION`、`datetime:YYYY-MM-DDTHH:MM[:SS]`、`signal:USR1` / `signal:SIGUSR1`、`signal:USR2` / `signal:SIGUSR2`、または `file:PATH` とします。その他の type、signal 名、大文字小文字違い、不正形式はエラーです。
- `duration:DURATION` は `time.ParseDuration` 互換とし、`0s` は即時成立、負値と不正値はエラーとします。複数の duration を指定でき、正の duration はすべて stdin の最初の非空 read 完了を同じ開始点として監視します。
- `datetime:YYYY-MM-DDTHH:MM[:SS]` はローカル時刻として起動時から監視します。秒は省略可能で、無効な日時、明示的な timezone、存在しない DST 時刻はエラーとします。複数の datetime を指定できます。
- duration は parse 後の値、datetime は解決後の instant が同じ条件なら equivalent として1つの latch/event を共有します。異なる値は独立した条件です。signal alias は正規化しますが、file path は正規化・実体同一性による重複排除を行いません。
- 条件引数の順序は自由で、`--or CONDITION` と `--or=CONDITION` の両方を受理します。`--or` は直前の condition と次の condition の間に必要で、先頭・末尾・連続・空値はエラーです。`--buffer-size` は条件や `--or` の前後に置け、複数指定時は最後の値を使います。
- 1つの argument 内で exact な ` && ` で連結した条件は AND group です。シェルに `&&` を解釈させないよう group 全体を quote します。group member は成立時に latch するため、成立順序に依存せず、file が成立後に削除されても gate は閉じません。
- `--version` は単独指定時に `dam <version>\n` を stdout へ出力して終了し、開発時の既定値は `dev` とします。リリースビルドでは `main.version` をリンク時に差し替えます。余分な引数付きの `--version` はエラーです。
- raw argument が完全一致する `-h` または `--help` は引数位置を問わず最優先で、複数指定でも help を一度だけ stdout へ出力して正常終了します。help 出力は末尾改行付きで、stdin を読まず、release monitor/readiness を開始しません。非完全一致の help 形式は通常の引数エラーです。help と `--version` や未知引数が併記された場合も help が優先され、引数エラー時に全文 help は自動出力しません。
- file 条件は全対応環境で `os.Stat` 相当の probe を使い symlink をたどります。regular file は成立、存在しない path と dangling symlink は未成立として待機します。directory、FIFO、device、symlink loop、権限エラー、その他の stat error は、ゲートが CLOSED の間だけ fatal とします。同一 path の反復指定は受理しますが、実体 path や大文字小文字による重複排除は行いません。polling 間隔と厳密な検知遅延は公開契約にしません。
- stdin を読む前に全 file 条件の初回 probe を完了します。初回結果に一つでも fatal があれば、別の file が regular、`0s`、または入力前に届いた signal が pending でも fatal を優先します。CLOSED 中は coordinator が OPEN を確定する前に報告を受理した fatal を、同じ判定時点で pending の release event より優先します。filesystem 上の変化時刻や probe 開始時刻の厳密な先後は保証しません。
- EOF が解放前に到達しても遅延を短縮しません。入力が一度もなければタイマーを開始しません。空 stdin の EOF は release condition を待たず正常終了し、file monitor を停止します。データ受信後の EOF は duration、datetime、signal、または file による解放までデータを保持します。
- duration、datetime、設定済み signal、file 条件は OR で、一度開いたゲートは再び閉じません。OPEN を確定したら全 file monitor と time monitor を停止し、進行中の probe の完了を待たず、その後に届いた結果を無視します。OPEN 後、最初の stdout write より前でもゲートは OPEN とみなします。
- 設定済みの SIGUSR1 / SIGUSR2 は最初の入力前から監視し、解放後もプロセス終了まで捕捉・無視します。duration、`0s`、datetime、または file が先に解放した場合も後続 signal でプロセスを終了させません。未設定のUSR signalは捕捉しません。Windows その他の未対応環境では signal を含まない duration / datetime / file の構成（組合せ含む）を受理しますが、signal 設定を含む構成は明示的な引数エラーとして拒否します。
- 入力をバイナリを含め byte-for-byte で保持し、通常のストリーム処理中の stdout はストリームデータ専用とします。help/version の情報出力は stdout、診断は stderr 専用です。
- 解放前に保持するストリームデータは実装内部の有界バッファに保持し、空き容量までは短い read も集約し、満杯後は通常のパイプのバックプレッシャーを利用します。バッファ容量は公開契約ではなく、現在の `preReleaseBufferSize` も内部実装詳細として扱います。
- タイマー待機中に stdin read がブロックしても、解放時刻に保持済みデータを書き出せる構造を維持します。進行中の read が返したデータは順序を崩さず、その後に転送します。
- signal と file 以外の外部解放イベント、繰り返すゲート遷移、設定可能な polling 間隔やバッファ、ディスクへの退避、initially-open モードは現在の対象外です。

## リポジトリ構成

- `cmd/dam/main.go`: 引数検証、遅延・注入 release ゲート、stdin/stdout 転送、終了コードを担当します。
- `cmd/dam/release_file.go`: release coordinator と、cross-platform な file 条件の初回 probe、CLOSED 中の polling、OPEN / 空 stdin EOF での停止を担当します。
- `cmd/dam/release_signal_unix.go`: Unix の SIGUSR1 / SIGUSR2 監視と、解放後も signal を捕捉し続けるライフサイクルを担当します。
- `cmd/dam/release_signal_windows.go` / `cmd/dam/release_signal_unsupported.go`: file-only / duration / datetime / help / version のビルドを維持しつつ、未対応環境で signal を含む設定を拒否します。
- `cmd/dam/main_test.go`: 時刻、EOF、バイナリ保持、バックプレッシャー、解放後転送、引数、I/O エラーの契約を固定します。
- `cmd/dam/release_file_test.go`: cross-platform な file parser / probe / polling、fatal error の優先順位、OPEN / 空 stdin EOF での monitor 停止を固定します。
- `cmd/dam/main_signal_unix_test.go`: プロセス分離した実 SIGUSR1 / SIGUSR2 配線と、解放後 signal の無害化を固定します。
- `cmd/dam/main_signal_windows_test.go`: Windows で file-only を許可し、signal を含む設定を拒否する契約を固定します。
- `.github/workflows/ci.yml`: Ubuntu、macOS、Windows で test/vet を実行し、Ubuntu で race test を実行します。
- `.github/workflows/release.yml`: Linux、macOS、Windows の amd64/arm64 向け成果物を作成します。

release workflow は `main.version` を注入してビルドし、Linux amd64 の成果物で `dam --version` の出力を確認します。

## サブエージェントの利用

軽微でないコード変更では、親エージェントは必ずサブエージェントを利用してください。

- 2ファイル以上を読む実装調査、不明な依存関係、呼び出し経路、影響範囲の列挙は `explorer` に委譲します。
- 振る舞いの変更、失敗テストの追加、実装、局所テストは、担当ファイル、完了条件、検証方法を明示して `coder` に一体で委譲します。
- 再現可能なテスト失敗の原因調査と修正、`gofmt`、局所テスト、クロスコンパイルなどの機械的な作業は `coder` に委譲します。
- 実装後は `reviewer` に、回帰、境界条件、互換性、セキュリティ、テスト不足を確認させます。
- 独立した作業は並列化し、担当範囲を重複させません。共有ワークツリー上の他者の変更を取り消してはいけません。
- 親エージェントは委譲前に Issue 本文、作業ツリー、公開仕様上の承認点だけを確認します。関連 Issue、履歴、呼び出し経路、影響範囲の詳細調査は `explorer` に任せ、同じ調査を繰り返しません。
- `coder` は TDD の red / green について、完全な実行コマンド、対象テスト名、red の意図した失敗理由、green の結果を報告します。
- 軽微な文言修正や単純な一行変更では、委譲は必須ではありません。

サブエージェントは報告に、根拠となるファイルと行、変更内容、実行したコマンドと結果を含めてください。親エージェントは同じ調査や実装を最初から繰り返さず、報告と差分を確認し、不足する箇所だけ追加調査します。

要求の解釈、公開 CLI 契約、並行処理モデル、OS 間互換性、データ形式、セキュリティ、変更間の競合に関する判断は親エージェントが保持します。局所検証は担当サブエージェントに任せ、親エージェントは統合後に変更内容に応じた全体検証を実行してから完了を報告してください。

## 開発プロセス

### テスト駆動開発

振る舞いを追加・変更する場合や不具合を修正する場合は、原則としてテスト駆動開発で進めます。

1. 期待する振る舞いを示す失敗テストを先に追加します。
2. テストを通す最小限の実装を行います。
3. テストを維持したまま、重複や分かりにくさを整理します。

既存テストで契約が十分に固定されているリファクタリングでは、変更前後に該当テストを実行します。コメントや文書だけの変更では、新しいテストの追加は必須ではありません。

### コミット

- 一つの機能、一つの修正、または同じ論点のレビュー指摘群を、一つの論理コミットにまとめます。
- 実装と、その振る舞いを保証するテストは同じコミットに含めます。
- 無関係な変更を同じコミットへ混ぜません。
- レビュー対応は、元の実装と区別できる独立したコミットにします。

### コードコメント

- 各 Go ファイルの先頭付近に、そのファイルが担当する責務を短く記載します。
- ビルドタグは必ずファイルの先頭に置き、責務コメントは `package` 宣言後に置きます。パッケージ全体の説明は package comment として記載します。
- 複雑な制御、並行処理、OS 固有処理、終了コードなどには、「何をしているか」ではなく「なぜ必要か」を説明します。
- コードをそのまま言い換えるコメントや、実装より強い保証を示すコメントは避けます。

## 検証

変更内容に応じて、少なくとも次を確認します。

- `gofmt` と `git diff --check`
- `go test ./...`
- 並行処理に関係する変更では `go test -race ./...`
- `go vet ./...`
- OS 固有コードに関係する変更では、対象 OS のテストまたはクロスコンパイル
- PR の GitHub Actions が Ubuntu、macOS、Windows、race の各ジョブで成功すること

## PR レビュー対応

- 指摘が現在の差分にも該当するか確認してから修正します。
- 対応コミットを push した後、変更内容とコミットをレビューコメントへ返信します。
- 対応テストと CI の成功を確認してから、該当するレビュースレッドだけを解決します。
