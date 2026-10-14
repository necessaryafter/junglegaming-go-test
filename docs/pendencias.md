# Limitações e verificações pendentes

A base está funcional, mas ainda não conclui o desafio. As decisões estão no
[índice de ADRs](../ARCHITECTURE.md); os comandos de execução e teste estão no
[README](../README.md#testes).

## O que já foi verificado

- Testes unitários: dinheiro, carteira/ledger, tipos externos, hash, estados, autorização e JWTs assinados.
- Integração real: PostgreSQL, IdP e SQS; migrations; composição e start/stop do Fx; cruzamento HTTP/SQS.
- Concorrência: 50 duplicatas, disputa de saldo, três pools independentes e outbox concorrente/abandonada.
- Matriz de reversões e referências: tipos permitidos, identidade, valores, duplicidade, concorrência, saldo insuficiente, replay, backoff e expiração. Veja a [cobertura detalhada](matriz-reversoes.md).
- O perfil `replicas` e `scripts/concurrency.py` exercitam três processos por HTTP, com reconciliação final.

Três pools de conexão não substituem a verificação de falhas entre três processos.

## O que falta verificar

- Matriz completa de recuperação com três processos independentes.
- Failpoints determinísticos entre commit e remoção da mensagem, e antes/depois do envio da outbox.
- Redrive/DLQ e recuperação de todos os cenários após restart.

## Limitações atuais

- SQS usa token de curta duração na mensagem e credenciais locais compartilhadas.
  Falta identidade durável de origem e demonstração de políticas segregadas por produtor.
- O modelo admite `FAILED`, mas não há política automática para persistir falhas
  permanentes de infraestrutura. Falhas transitórias fazem rollback/503 ou retry;
  mensagens inválidas ou sem autorização seguem retry/redrive.
- Faltam histogramas, métricas completas de SQS/DLQ e conflitos, dashboards e tracing.
- As validações financeiras no banco agregam dados por carteira; seu custo precisa ser medido em carga.
- O usuário local é proprietário do banco. Em produção, separar migration owner
  de runtime role, sem DDL, superuser ou permissão para desativar triggers.
