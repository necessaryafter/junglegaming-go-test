# Arquitetura

As decisões estão registradas em ADRs curtas, com a decisão e seu motivo.

- [0001 - Monólito modular](docs/adr/0001-monolito-modular.md)
- [0002 - Dinheiro exato](docs/adr/0002-dinheiro-exato.md)
- [0003 - Transação financeira e locks](docs/adr/0003-transacoes-e-concorrencia.md)
- [0004 - Idempotência persistente e inbox](docs/adr/0004-idempotencia-e-inbox.md)
- [0005 - Referências pendentes e reversões](docs/adr/0005-referencias-e-reversoes.md)
- [0006 - Identidade verificada e isolamento por provedor](docs/adr/0006-autenticacao-e-autorizacao.md)
- [0007 - Outbox com entrega pelo menos uma vez](docs/adr/0007-outbox-e-eventos.md)
- [0008 - Composição e ciclo de vida com Fx](docs/adr/0008-fx-e-ciclo-de-vida.md)
- [0009 - Diagnóstico e reconciliação](docs/adr/0009-observabilidade-e-reconciliacao.md)
- [0010 - Falhas permanentes e estado FAILED](docs/adr/0010-falhas-permanentes.md)
- [0011 - Migrations com golang-migrate](docs/adr/0011-migrations.md)
- [0012 - Permissões do banco por responsabilidade](docs/adr/0012-permissoes-do-banco.md)

HTTP usa o IdP externo Keycloak; SQS usa políticas IAM segregadas e assinaturas
duráveis Ed25519 por produtor, conforme a [ADR 0006](docs/adr/0006-autenticacao-e-autorizacao.md).
O ambiente local executa MiniStack com avaliação IAM habilitada.

## Escopo e limites operacionais

- **Validação financeira:** triggers diferidos conferem saldo, versão e ledger
  no commit. Uma BET com movimento agenda quatro verificações; as agregações
  têm custo proporcional ao histórico e ao plano SQL. Os
  [relatórios de carga](docs/testes/carga.md) e a
  [comparação dos índices](docs/testes/otimizacao-validacoes.md) registram os
  custos observados. As amostras locais não estabelecem capacidade máxima nem
  ganho sustentado da migration 4.
- **Outbox:** a publicação é assíncrona e pode ficar atrás da entrada HTTP sob
  carga. O evento permanece no PostgreSQL para retry; aceite HTTP e aceite pelo
  SQS são etapas distintas. As medições registram a espera pela drenagem.
- **FAILED:** representa perda irrecuperável comprovada por investigação
  operacional. O serviço valida essa política, mas não inclui classificador
  automático nem endpoint administrativo para registrar a decisão. Falhas
  recuperáveis seguem rollback, retry ou redrive. Veja a
  [política de falhas permanentes](docs/adr/0010-falhas-permanentes.md).
- **Observabilidade:** a entrega inclui logs JSON, histogramas, métricas de
  SQS/DLQ, Prometheus, Grafana e o perfil Loki + Alloy. Alertas provisionados e
  tracing distribuído estão fora do escopo desta entrega. O
  [catálogo de diagnóstico](docs/guias/observabilidade.md) descreve os limites
  de coleta e correlação.
- **Credenciais e operação:** os valores do Compose são locais. Ambientes
  externos precisam fornecer identidades, segredos e políticas próprios. O
  bootstrap preserva as chaves locais e reaplica grants; rotação e revogação
  permanentes exigem adaptar o provisionamento. Veja os
  [procedimentos de identidade](docs/adr/0006-autenticacao-e-autorizacao.md#operação-e-limites)
  e a [separação de permissões do banco](docs/adr/0012-permissoes-do-banco.md).

## Evidências

A [reprodução documentada](docs/testes/evidencias-entrega.md) registra checkout
limpo, dependências reais, autorização, concorrência e recuperação. Foi usada a
sequência de Podman Compose; Docker Compose v2 não foi executado diretamente
nesse ambiente. Os cenários e comandos estão no [índice de testes](docs/README.md#testes).
