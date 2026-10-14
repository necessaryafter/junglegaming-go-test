# ADR 0006 — Identidade verificada e isolamento por provedor

## Decisão

Usar Keycloak com `client_credentials` e go-oidc para validar assinatura RS256,
issuer, audience e expiração. Verificar também `nbf`, subject e roles.
A role `provider` habilita a identidade `providerId`; `internal` permite operações
de carteira. Autorizar no caso de uso antes do repositório e filtrar consultas
externas pelo provedor autenticado. `identity.Principal` não carrega JWT ou HTTP.

HTTP e SQS seguem a mesma autorização. Hoje, SQS exige `AccessToken` na mensagem.
Tokens podem expirar antes da entrega; identidade durável de origem e políticas
segregadas do broker ainda precisam substituir essa solução inicial.

## Motivo

O token verificado estabelece a identidade; o `providerId` enviado no corpo
não comprova quem está chamando. Autorizar no caso de uso mantém a mesma proteção
em HTTP e SQS, e filtrar pelo provedor impede acesso a transações de terceiros.

## Verificação da entrada SQS

A [matriz de redrive](../../internal/wagering/worker/dlq_integration_test.go) cobre atributo AccessToken ausente,
assinatura adulterada e JWT válido de provider-b enviado junto de uma operação de
provider-a. O terceiro caso comprova a verificação do token antes de exercitar a
autorização por provedor. Cada mensagem passa pelos cinco recebimentos e pelo
backoff de produção, chega à DLQ e mantém o corpo e os atributos originais, sem
conclusão da inbox, alterações financeiras ou credenciais nos logs.
