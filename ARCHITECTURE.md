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

Veja [limitações e verificações pendentes](docs/pendencias.md).
