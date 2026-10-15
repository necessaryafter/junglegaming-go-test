# Limitações e verificações pendentes

A base está funcional, mas ainda não conclui o desafio. As decisões estão no
[índice de ADRs](../ARCHITECTURE.md); os comandos de execução e teste estão no
[README](../README.md#testes).

## O que já foi verificado

- Testes unitários: dinheiro, carteira/ledger, tipos externos, hash, estados, autorização e JWTs assinados.
- Integração real: PostgreSQL, IdP e SQS; migrations; composição e start/stop do Fx; cruzamento HTTP/SQS.
- Concorrência: 50 duplicatas, disputa de saldo, três pools independentes e outbox concorrente/abandonada.
- Matriz de reversões e referências: tipos permitidos, identidade, valores, duplicidade, concorrência, saldo insuficiente, replay, backoff e expiração. Veja a [cobertura detalhada](matriz-reversoes.md).
- Recuperação do consumidor após commit e antes de delete: processo encerrado à força, expiração real de visibilidade e replay em outro processo, sem novos efeitos financeiros. Veja o [cenário reproduzível](recuperacao-consumidor.md).
- Recuperação do publisher antes do envio e após aceite pelo SQS, antes da confirmação SQL: queda forçada, expiração real do lease e retomada em outro processo, preservando evento e estado financeiro. Veja os [cenários reproduzíveis](recuperacao-publisher.md).
- Mensagem com campo desconhecido chega à DLQ após cinco recebimentos reais, com backoff de produção, sem alteração financeira nem conclusão da inbox. Provisionamento de redrive também conferido; veja o [cenário reproduzível](redrive-dlq.md).
- Mensagens sem AccessToken, com assinatura inválida e com token legítimo de outro provedor chegam à DLQ, sem efeitos financeiros ou conclusão da inbox, preservando o atributo de credencial quando presente. Cobertura na [matriz de redrive](redrive-dlq.md).
- Falha persistente de escrita no PostgreSQL: cinco rollbacks completos até a DLQ, remoção da falha e reprocessamento em outro processo, seguido de duplicata recebida pela aplicação sem novo débito. Veja o [cenário de recuperação](recuperacao-banco-dlq.md).
- Três aplicações Fx simultâneas: concorrência financeira, isolamento entre carteiras, reentrega HTTP/SQS, disputa da outbox, retomada e expiração de referências, DLQ e reprocessamento após restart. Veja a [matriz de três processos](recuperacao-tres-processos.md).
- O perfil `replicas` e `scripts/concurrency.py` exercitam três processos por HTTP, com reconciliação final.

A matriz automatizada confere três PIDs distintos, prontidão das aplicações e três pools identificados separadamente no PostgreSQL.

Os cenários de recuperação e restart da matriz estão automatizados com
infraestrutura real. A cobertura e os limites estão descritos na
[matriz de três processos](recuperacao-tres-processos.md).

## Limitações atuais

- SQS usa token de curta duração na mensagem e credenciais locais compartilhadas.
  Falta identidade durável de origem e demonstração de políticas segregadas por produtor.
- A [política de `FAILED`](adr/0010-falhas-permanentes.md) restringe esse estado a
  perda irrecuperável de dados confirmada operacionalmente. O fluxo atual não tem
  classificador automático nem ferramenta administrativa para registrar essa decisão.
  Falhas recuperáveis fazem rollback/503 ou retry; mensagens inválidas ou sem
  autorização seguem retry/redrive.
- Faltam histogramas, métricas completas de SQS/DLQ e conflitos, dashboards e tracing.
- As validações financeiras no banco agregam dados por carteira; seu custo precisa ser medido em carga.
- O Compose separa administrador, migrator e runtime, com
  [permissões verificadas em PostgreSQL real](adr/0012-permissoes-do-banco.md).
  Ambientes externos devem provisionar os mesmos privilégios e fornecer segredos
  próprios; os valores locais de `.env.example` são apenas de desenvolvimento.
