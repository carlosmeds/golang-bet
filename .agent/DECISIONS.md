# Decisões propostas

Estas decisões são propostas de implementação para revisão; `SPEC.md` prevalece em caso de divergência. Detalhes definitivos, limitações e trade-offs entram em `ARCHITECTURE.md` durante a execução.

| ID | Decisão | Motivo e verificação |
| --- | --- | --- |
| D01 | `Money` usa `int64` em centavos, moeda ISO 4217 e parser decimal manual estrito. | Evita ponto flutuante e arredondamento. Testar limites, overflow em parse/soma/subtração/negação, zero e moeda incompatível. |
| D02 | PostgreSQL com `pgx`, SQL explícito e transação coordenada pelo caso de uso. | Permite inspecionar locks, limites da transação e constraints. Repositórios recebem o mesmo `pgx.Tx`. |
| D03 | Lock pessimista `SELECT ... FOR UPDATE` por carteira, índices/constraints como segunda linha de defesa. | Serializa escritores da mesma carteira sem travar carteiras diferentes. Validar com três processos e `go test -race`. |
| D04 | `wager_transactions` separa origem `INTERNAL`/`EXTERNAL` por constraints e índice único de abertura por carteira. | OPENING não pode entrar por HTTP/SQS nem duplicar crédito inicial. Campos externos são obrigatórios apenas para origem externa. |
| D05 | Transações processadas persistem `result_balance_amount`, `result_balance_currency`, status e failure code; índice único por chave e ID externos dentro do provedor. | Replays reproduzem resultado original, detectam hash diferente e bloqueiam nova chave para a mesma operação. |
| D06 | Hash SHA-256 sobre JSON canônico com chaves ordenadas e apenas campos de negócio: providerId, externalTransactionId, playerId, walletId, roundId, gameId, kind, money e referenceExternalTransactionId quando presente. Normalizar UUID e moeda; decimal aceito é serializado como duas casas. | Mesma operação via HTTP/SQS produz mesmo hash. Chave de idempotência, envelope e transporte ficam fora. Definir vetores de teste cruzados. |
| D07 | Uma reversão processada por referência, com exclusão mútua entre REFUND e ROLLBACK quando a referência é BET; ROLLBACK de WIN/REFUND ocorre no máximo uma vez. | Impede crédito duplo, inclusive em corrida. Um rollback de REFUND não reabre a possibilidade de refund da BET. |
| D08 | Referência ausente ou pendente usa `PENDING_REFERENCE` com `next_attempt_at`, contador, lease e TTL de 24 horas; backoff exponencial limitado a 5 minutos. | Retomada por outra instância. O TTL prevalece; exaustão gera `REFERENCE_NOT_FOUND` ou `REFERENCE_UNRESOLVED`, conforme o caso. Configuração e testes fixarão o limite de tentativas. |
| D09 | Falhas de negócio confirmadas têm códigos estáveis; `INSUFFICIENT_FUNDS_BET` difere de `INSUFFICIENT_FUNDS_REVERSAL`. Erros de infraestrutura transitórios não finalizam a operação. | Preserva semântica para cliente, auditoria e retry. Códigos adicionais incluem `REFERENCE_NOT_FOUND`, `REFERENCE_UNRESOLVED`, `REFERENCE_FAILED`, `REFERENCE_MISMATCH`, `ALREADY_REVERSED`, `WALLET_MISMATCH`, `CURRENCY_MISMATCH`. |
| D10 | Inbox SQS e efeito financeiro compartilham um commit; messageId do envelope identifica a mensagem e seu hash é comparado no replay. | Delete só após commit. Duplicata com corpo diferente é erro permanente auditável e enviada à DLQ. |
| D11 | Outbox usa snapshots de eventos tipados, `eventId` estável, claim SQL com `SKIP LOCKED` e lease; publicação ocorre após commit. | Crash após envio pode republicar o mesmo `eventId`. Consumidores externos devem deduplicar por `eventId`. |
| D12 | Fila de entrada SQS FIFO é acessível só ao produtor interno autenticado; chamadas HTTP recebem identidade de provedor pelo IdP, que a vincula ao pedido encaminhado. | Mensagens SQS não carregam uma identidade de remetente confiável por si. A política de broker impede provedores de publicar diretamente; testes e documentação verificam essa fronteira. |
| D13 | Endpoints de negócio validam JWT OIDC por JWKS e autorizam o `providerId` pelo claim; operações de carteira usam escopo interno. | Evita acesso cruzado em consultas/replays. Dados de outro provedor não devem ser expostos. |
| D14 | Saúde: liveness local; readiness consulta PostgreSQL e SQS com prazos curtos. Reconciliação lê carteira e ledger numa transação read-only de snapshot consistente. | Sinaliza dependências e evita falso desvio sob escrita concorrente. Divergência é registrada em resposta, log e métrica, sem corrigir saldo. |
| D15 | SQS FIFO usa `MessageGroupId=walletId` na entrada e `aggregateId` na saída, `MessageDeduplicationId=messageId/eventId`; nenhuma garantia depende disso. | Permite paralelismo entre carteiras e reentrega segura. HTTP e SQS ainda competem no lock da carteira. |
| D16 | Saldo inicial zero cria só carteira; saldo positivo cria OPENING, ledger e dois eventos no mesmo commit, mantendo versão 1. | Alinha versão e ledger desde a criação. Reabertura do mesmo `(playerId,currency)` retorna conflito. |

## Pontos a fechar durante revisão de contratos

- Forma exata dos claims OIDC, papéis de serviço e contas locais no realm provisionado.
- Códigos HTTP e envelopes de erro para rejeição, pendência, conflito e indisponibilidade.
- Esquema e política de versionamento dos eventos; nome/atributos da fila outbound.
- Número de tentativas de referência, consumer e publisher, visibility timeout e prazo de shutdown. Os limites devem ser configuráveis e testados.
- Versões exatas de Go, containers e dependências devem ser fixadas antes da implementação e repetidas em `go.mod`, Dockerfile e documentação.

## Execução autônoma e coordenação

| ID | Decisão | Motivo e verificação |
| --- | --- | --- |
| D17 | O módulo Go será `wagering`; pacotes internos importarão `wagering/internal/...`. | Identidade local independente, estável entre worktrees e sem acoplamento a hospedagem remota. |
| D18 | O Lead coordena uma Run Orca com tarefas DAG; workers usam worktrees isolados e commits locais por tarefa. | Evita edições concorrentes no mesmo checkout e torna revisão/integracão auditável. Não haverá push remoto. |
| D19 | Worker normal recebe seu item em `TASKS.yaml`, REQ IDs em `REQUIREMENTS.md`, decisões e handoffs pertinentes. Não recebe nem lê `SPEC.md`; revisor independente final recebe a cópia local imutável. | Reduz contexto e preserva uma fonte autoritativa para resolução de ambiguidade pelo Lead. |
| D20 | Modelos são selecionados por família e tier, sem IDs inventados. Codex `gpt-6.1-sol` aparece no cache local; Claude aceita o alias `sonnet`; Antigravity usa o default configurado quando lançado. A seleção efetiva será confirmada pelo recibo Orca. | Usa modelos econômicos onde adequados e verifica disponibilidade real. Astra fica reservado a falhas substantivas repetidas. |
| D21 | Cada handoff contém TASK, STATUS, COMMIT, IMPLEMENTED, FILES CHANGED, TESTS EXECUTED, DECISIONS / ASSUMPTIONS, KNOWN RISKS e NOTES FOR DEPENDENT TASKS. | O Lead só integra após ler o handoff, inspecionar o diff e verificar testes relevantes. |
| D22 | Não há gates humanos para decisões normais. Achados de revisão geram tarefas de reparo na mesma Run; Lead só para por bloqueio externo genuíno. | Executa a autorização explícita da fase de implementação. |
| D23 | Go 1.23.0 e Fx v1.24.0 foram fixados no bootstrap inicial. | É a versão Go instalada localmente; T12 repete a versão no Dockerfile. |
| D24 | O Lead assumiu T01 após três falhas de detecção de readiness do Codex TUI no Orca; T02 foi redistribuída para Claude Sonnet/high pelo mesmo problema. | Evita repetir tentativas sem sinal de progresso. Os commits continuam por tarefa, e os workers restantes permanecem supervisionados. |
